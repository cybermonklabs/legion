package docs

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

func TestBackfillAnchorBlocksPinsResolvableLegacyRowsOnce(t *testing.T) {
	service, artifactID := newTestService(t)
	seedServiceText(t, service, artifactID, "Introduction.\n\nTarget passage.\n")
	askID := insertAnchoredAsk(t, service, artifactID, "Target passage")
	commentID := insertAnchoredComment(t, service, artifactID, "Target passage")

	first, err := service.BackfillAnchorBlocks(context.Background())
	if err != nil {
		t.Fatalf("backfill anchor blocks: %v", err)
	}
	if first.Asks != 1 || first.Comments != 1 || first.Skipped != 0 {
		t.Fatalf("first backfill = %#v, want both rows updated", first)
	}

	wantBlockID, err := service.BlockForQuote(context.Background(), artifactID, "Target passage")
	if err != nil {
		t.Fatalf("resolve target block: %v", err)
	}
	for _, row := range []struct {
		id    string
		table string
	}{{askID, "asks"}, {commentID, "comments"}} {
		var encoded []byte
		if err := service.store.Pool.QueryRow(context.Background(), "select anchor from "+row.table+" where id = $1", row.id).Scan(&encoded); err != nil {
			t.Fatalf("load %s anchor: %v", row.table, err)
		}
		var anchor model.Anchor
		if err := json.Unmarshal(encoded, &anchor); err != nil {
			t.Fatalf("decode %s anchor: %v", row.table, err)
		}
		if anchor.BlockID == nil || *anchor.BlockID != wantBlockID {
			t.Fatalf("%s block_id = %v, want %q", row.table, anchor.BlockID, wantBlockID)
		}
	}

	second, err := service.BackfillAnchorBlocks(context.Background())
	if err != nil {
		t.Fatalf("repeat backfill anchor blocks: %v", err)
	}
	if second.Asks != 0 || second.Comments != 0 || second.Skipped != 0 {
		t.Fatalf("second backfill = %#v, want idempotent no-op", second)
	}
}

// A settlement's anchor refresh owns two fields of an anchor, the quote and the orphan flag. It
// reads each open row's anchor and writes it back, so a write of the whole column erases whatever
// another writer put in that anchor after the refresh read it - here the block id the one-time
// BackfillAnchorBlocks repair pins. The advisory lock holds the settlement in its update of the
// asks, after it read the rows and before it updates the comments, which is the window a loaded
// CI runner hit on its own.
//
// The shape of the test changed with INFRA-636's ruling that the backfill takes the owner lock of
// each row it writes. Before it, the backfill wrote on the pool, so it could pin the comment
// inside that window and the test ran it there, under the name it had then
// ("...KeepsABlockIDPinnedAfterItReadTheRow"); now both writers take the row's owner lock - the
// settlement through its version write (lockArtifactOwner), the backfill through
// refs.LockItemOwner - so the interleaving cannot happen at all and the backfill parks on that
// lock until the settlement commits. What the test proves is that they serialise on the owner and
// that both writes stand: the refresh clears the orphan flag, the backfill pins the block, and
// the row carries both at the end.
func TestAnchorRefreshAndBackfillSerialiseOnTheRowsOwner(t *testing.T) {
	const holdKey = int64(4815162342)
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "Introduction.\n\nTarget passage.\n\nOther passage.\n")
	askID := insertAnchoredAsk(t, service, artifactID, "Other passage")
	commentID := insertAnchoredComment(t, service, artifactID, "Target passage")

	// Both rows are stale: their marks are live but their anchors say orphaned, so the refresh
	// has a reason to write each one. The ask's block is already pinned, which is what keeps the
	// backfill off the row the settlement locks first.
	if _, err := service.store.Pool.Exec(context.Background(), `
		update asks
		set anchor = jsonb_set(jsonb_set(anchor, '{orphaned}', to_jsonb(true)), '{block_id}', to_jsonb('already-pinned'::text), true)
		where id = $1
	`, askID); err != nil {
		t.Fatalf("stale the ask anchor: %v", err)
	}
	if _, err := service.store.Pool.Exec(context.Background(), `
		update comments set anchor = jsonb_set(anchor, '{orphaned}', to_jsonb(true)) where id = $1
	`, commentID); err != nil {
		t.Fatalf("stale the comment anchor: %v", err)
	}
	wantBlockID, err := service.BlockForQuote(context.Background(), artifactID, "Target passage")
	if err != nil {
		t.Fatalf("resolve target block: %v", err)
	}

	if _, err := service.store.Pool.Exec(context.Background(), `
		create function dispatch_test_hold_anchor_refresh() returns trigger language plpgsql as $$
		begin
			perform pg_advisory_xact_lock(4815162342);
			return new;
		end;
		$$
	`); err != nil {
		t.Fatalf("create anchor refresh hold function: %v", err)
	}
	if _, err := service.store.Pool.Exec(context.Background(), `
		create trigger dispatch_test_hold_anchor_refresh
		before update on asks for each row
		when (new.anchor is distinct from old.anchor)
		execute function dispatch_test_hold_anchor_refresh()
	`); err != nil {
		t.Fatalf("create anchor refresh hold trigger: %v", err)
	}
	t.Cleanup(func() {
		_, _ = service.store.Pool.Exec(context.Background(), `drop trigger if exists dispatch_test_hold_anchor_refresh on events`)
		_, _ = service.store.Pool.Exec(context.Background(), `drop function if exists dispatch_test_hold_anchor_refresh()`)
	})

	// A content change is what makes the settlement write a version, and a version write is what
	// refreshes the anchors.
	editLiveTree(t, service, artifactID, replaceRun("Introduction.", "Intro."))
	settleCtx, cancelSettle := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelSettle()
	if err := service.waitForPendingUpdates(settleCtx, artifactID); err != nil {
		t.Fatalf("wait for live updates to reach persistence: %v", err)
	}
	if err := service.waitForDurableAppends(settleCtx, artifactID); err != nil {
		t.Fatalf("wait for live updates to become durable: %v", err)
	}
	state := service.room(artifactID)
	state.mu.Lock()
	generation := state.roomGeneration
	state.mu.Unlock()

	hold, err := service.store.Pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin anchor refresh hold: %v", err)
	}
	defer hold.Rollback(context.Background())
	if _, err := hold.Exec(context.Background(), `select pg_advisory_xact_lock($1)`, holdKey); err != nil {
		t.Fatalf("hold the anchor refresh: %v", err)
	}

	settled := make(chan struct{})
	go func() {
		defer close(settled)
		service.settleRoom(artifactID, generation)
	}()
	waitForHeldAnchorRefresh(t, service, holdKey)

	// The backfill runs beside the held settlement, in a goroutine, because it no longer runs
	// past it: it takes the comment's owner lock, which the settlement's version write holds
	// (lockArtifactOwner, refs.LockItemOwner), so it parks there until the settlement commits.
	// Its own wait is the proof, read off pg_locks rather than off a clock.
	type backfillResult struct {
		report AnchorBlockBackfill
		err    error
	}
	backfilled := make(chan backfillResult, 1)
	go func() {
		report, err := service.BackfillAnchorBlocks(context.Background())
		backfilled <- backfillResult{report: report, err: err}
	}()
	waitForHeldOwnerLock(t, service, store.IssueOwner("DOC-1"))

	if err := hold.Rollback(context.Background()); err != nil {
		t.Fatalf("release the anchor refresh: %v", err)
	}
	<-settled

	var backfill AnchorBlockBackfill
	select {
	case result := <-backfilled:
		if result.err != nil {
			t.Fatalf("backfill anchor blocks after the settlement: %v", result.err)
		}
		backfill = result.report
	case <-time.After(30 * time.Second):
		t.Fatal("the backfill never finished after the settlement released the comment's owner lock")
	}
	if backfill.Asks != 0 || backfill.Comments != 1 {
		t.Fatalf("backfill after the settlement = %#v, want the comment alone", backfill)
	}

	var encoded []byte
	if err := service.store.Pool.QueryRow(context.Background(), `select anchor from comments where id = $1`, commentID).Scan(&encoded); err != nil {
		t.Fatalf("load comment anchor: %v", err)
	}
	var anchor model.Anchor
	if err := json.Unmarshal(encoded, &anchor); err != nil {
		t.Fatalf("decode comment anchor: %v", err)
	}
	if anchor.BlockID == nil || *anchor.BlockID != wantBlockID {
		t.Fatalf("comment block_id after the settlement = %v, want %q", anchor.BlockID, wantBlockID)
	}
	if anchor.Orphaned {
		t.Fatalf("comment anchor stayed orphaned, so the settlement never refreshed it")
	}
}

// waitForHeldAnchorRefresh returns once the settlement is inside its anchor refresh, waiting on
// the advisory lock the test holds. The lock's own wait queue is the settlement's observable
// position; nothing here assumes how long a step takes.
func waitForHeldAnchorRefresh(t *testing.T, service *Service, key int64) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if anchorRefreshWaiters(t, service.store, key) > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the settlement never reached its anchor refresh")
}

// waitForHeldOwnerLock returns once a transaction is waiting on owner's lock, which is where a
// writer that takes it parks while another transaction holds it (store.LockOwner). The lock's own
// wait queue is that writer's observable position, read as waitForHeldAnchorRefresh reads the
// test's own key, so nothing here waits a fixed length for a step to happen; the deadline bounds
// the wait and the failure names the lock it waited for.
func waitForHeldOwnerLock(t *testing.T, service *Service, owner store.Owner) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if ownerLockWaiters(t, service.store, owner) > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no transaction waited on %s %s's owner lock", owner.Kind, owner.Key)
}

// anchorRefreshWaiters counts the backends of the test's database that wait on the advisory lock
// of key, as waitForHeldAnchorRefresh polls it, and ownerLockWaiters those that wait on owner's
// lock, as waitForHeldOwnerLock polls it. Each reads its own database alone (storetest.Waiting):
// every test's database is a clone of one template on a server other tests share, and the same
// key names the same lock in each, so a waiter in another test's database would count.
func anchorRefreshWaiters(t *testing.T, database *store.Store, key int64) int {
	t.Helper()
	return storetest.WaitersOn(t, database, storetest.AdvisoryLock(key))
}

func ownerLockWaiters(t *testing.T, database *store.Store, owner store.Owner) int {
	t.Helper()
	return storetest.WaitersOn(t, database, storetest.OwnerLock(owner))
}

// Both lock-wait probes above count their own database's waiters alone: a waiter on the lock in
// another database of the server counts for nothing, and one in its own counts once, the control
// that shows the probe sees a waiter at all. The key and the owner are any: each probe reads the
// lock it is given. The key's two halves are both nonzero, and before its own waiter the test's
// database holds a waiter on each of two other keys, one with the key's high half and one with its
// low half, which must count for nothing too: so the anchor probe counts the key only by reading
// pg_locks' classid and objid both.
func TestTheAnchorWaitsCountTheirOwnDatabasesWaitersAlone(t *testing.T) {
	const key = int64(3)<<32 | 7
	owner := store.IssueOwner("probe")
	for _, probe := range []struct {
		lock   storetest.Lock
		count  func(*testing.T, *store.Store) int
		halves []storetest.Lock
	}{
		{storetest.AdvisoryLock(key), func(t *testing.T, database *store.Store) int { return anchorRefreshWaiters(t, database, key) },
			[]storetest.Lock{storetest.AdvisoryLock(int64(3)<<32 | 8), storetest.AdvisoryLock(int64(4)<<32 | 7)}},
		{storetest.OwnerLock(owner), func(t *testing.T, database *store.Store) int { return ownerLockWaiters(t, database, owner) }, nil},
	} {
		t.Run(probe.lock.String(), func(t *testing.T) {
			database := storetest.Open(t)
			elsewhere := storetest.Queue(t, storetest.Open(t), probe.lock)
			if waiting := probe.count(t, database); waiting != 0 {
				t.Fatalf("the probe counted %d waiters on %s with none in its own database and backend %d waiting in another, want 0", waiting, probe.lock, elsewhere)
			}
			for _, half := range probe.halves {
				beside := storetest.Queue(t, database, half)
				if waiting := probe.count(t, database); waiting != 0 {
					t.Fatalf("the probe counted %d waiters on %s with none on it in its own database and backend %d waiting there on %s, which shares one half of its key, want 0", waiting, probe.lock, beside, half)
				}
			}
			here := storetest.Queue(t, database, probe.lock)
			if waiting := probe.count(t, database); waiting != 1 {
				t.Fatalf("the probe counted %d waiters on %s with backend %d waiting in its own database, want 1", waiting, probe.lock, here)
			}
		})
	}
}

func TestBlockForQuoteLeavesTopLevelCrossBlockAnchorsUnpinned(t *testing.T) {
	service, artifactID := newTestService(t)
	seedServiceText(t, service, artifactID, "One.\n\nTwo.\n")

	blockID, err := service.BlockForQuote(context.Background(), artifactID, "One. Two.")
	if err != nil {
		t.Fatalf("resolve cross-block quote: %v", err)
	}
	if blockID != "" {
		t.Fatalf("cross-block quote block_id = %q, want empty", blockID)
	}
}
