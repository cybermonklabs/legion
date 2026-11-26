package docs

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest/lockstep"
	"github.com/sjawhar/envoy/internal/testcleanup"
)

func TestBackfillAnchorBlocksPinsResolvableLegacyRowsOnce(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	testcleanup.Add(t, func() {
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
		var waiting int
		if err := service.store.Pool.QueryRow(context.Background(), `
			select count(*) from pg_locks
			where locktype = 'advisory' and not granted
				and database = (select oid from pg_database where datname = current_database())
				and ((classid::bigint << 32) | objid::bigint) = $1
		`, key).Scan(&waiting); err != nil {
			t.Fatalf("read the anchor refresh hold: %v", err)
		}
		if waiting > 0 {
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
		var waiting int
		if err := service.store.Pool.QueryRow(context.Background(), `
			select count(*) from pg_locks
			where locktype = 'advisory' and not granted and objsubid = 2
				and database = (select oid from pg_database where datname = current_database())
				and classid = ($2::bigint & 4294967295)::oid
				and objid = (hashtext($1)::bigint & 4294967295)::oid
		`, owner.LockKey(), int64(store.OwnerLockNamespace)).Scan(&waiting); err != nil {
			t.Fatalf("read the waiters on %s %s's owner lock: %v", owner.Kind, owner.Key, err)
		}
		if waiting > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no transaction waited on %s %s's owner lock", owner.Kind, owner.Key)
}

func TestBlockForQuoteLeavesTopLevelCrossBlockAnchorsUnpinned(t *testing.T) {
	t.Parallel()
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

// The anchor block backfill pins a legacy comment anchor's block and states its anchored_in in one
// transaction (T17b of the issue-tree plan): the comment's row, which its update locks,
// then the tuple through the store's writer at the sentinel. It takes the row's owner lock first,
// in the row's own transaction (backfillAnchorBlock): a settlement holds that owner's lock while it
// refreshes the anchors, and the two meet at the lock and the row alone
// (TestAnchorRefreshAndBackfillSerialiseOnTheRowsOwner). The issue's owner lock is held, then the
// comment's row, while one run of the backfill runs on a connection of its own (lockstep.Follow),
// over a document service on that connection: the run queues behind the owner lock at the
// statement that takes it (store.OwnerLockStatement), then behind that row at its update, holding
// no lock on the store's tables, and never behind the project's row. The comment's node then
// states one anchored_in to the pinned block's node, so the run followed is the one that committed.
func TestTheAnchorBlockBackfillTakesTheRowThenTheTuple(t *testing.T) {
	ctx := context.Background()
	service, artifactID := newTestService(t)
	seedServiceText(t, service, artifactID, "Introduction.\n\nTarget passage.\n")
	commentID := insertAnchoredComment(t, service, artifactID, "Target passage")
	database := service.store
	var refKey string
	if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `select ref_key from artifacts where id = $1::uuid`, artifactID).Scan(&refKey); err != nil {
			return err
		}
		node, _ := graphstore.CommentFacts(nil, commentID, model.Actor{Kind: "user", ID: "alice"}, time.Now().UTC(), graphstore.NoEvent, "", "", "DOC", nil)
		if err := graphstore.Write(ctx, tx, []graphstore.Node{node}, nil); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `update comments set node_kind = $2, node_address = $1::text where id = $1::uuid`, commentID, graphstore.KindComment)
		return err
	}); err != nil {
		t.Fatalf("give the legacy comment its node: %v", err)
	}
	wantBlock, err := service.BlockForQuote(ctx, artifactID, "Target passage")
	if err != nil {
		t.Fatalf("resolve the target block: %v", err)
	}
	runner := storetest.SingleConnection(t, database)
	backfiller := newTestServiceOn(t, runner)
	pid := backendOf(t, runner)

	owner := lockstep.HoldOwner(t, database, &lockstep.Hold{
		Name: "issue DOC-1's owner lock", Step: lockstep.StepOwner, Taken: true,
		Statement: lockstep.OwnerLockStatement,
		Unlocked:  []string{"graph_nodes", "graph_tuples"},
	}, store.IssueOwner("DOC-1"))
	row := lockstep.HoldRow(t, database, &lockstep.Hold{
		Name: "the comment's row", Step: lockstep.StepProjection, Taken: true,
		Statement: "update comments c set anchor = jsonb_set(",
		Unlocked:  []string{"graph_nodes", "graph_tuples"},
	}, `select 1 from comments where id = $1::uuid for update`, commentID)
	project := lockstep.HoldRow(t, database, &lockstep.Hold{Name: "project DOC's row", Step: lockstep.StepEvent},
		`select 1 from projects where key = $1 for no key update`, "DOC")
	done := make(chan error, 1)
	go func() {
		backfill, err := backfiller.BackfillAnchorBlocks(context.Background())
		if err == nil && backfill.Comments != 1 {
			err = fmt.Errorf("the backfill pinned %d comments, want the one", backfill.Comments)
		}
		done <- err
	}()
	lockstep.Follow(t, database, "the anchor block backfill", pid, done, []*lockstep.Hold{owner, row, project})

	held, err := graphstore.CurrentOut(ctx, database.Pool, graphstore.KindComment, commentID, []string{graphstore.TupleAnchoredIn})
	if err != nil {
		t.Fatalf("read the comment's anchored_in: %v", err)
	}
	if len(held) != 1 || held[0].ToID != refKey+"#"+wantBlock || held[0].Who != AnchorBlockBackfillActor {
		t.Errorf("the comment states anchored_in %+v, want one to %s#%s written by %v: the run followed is not one that committed",
			held, refKey, wantBlock, AnchorBlockBackfillActor)
	}
}

// The anchor refresh states a comment's anchored_in in the store's lock order (T17b of
// this branch's plan): inside the settlement's transaction, after the document owner's
// lock, which the settlement takes first (lockArtifactOwner), the update of the comments row it
// refreshes, then that comment's comment.anchor_refreshed, and then the tuple that event causes.
// The comment's anchor names its block and stands orphaned though its mark is in the document, so
// the refresh finds it again and states the block. The issue's owner lock and the comment's row
// are each held while one settlement runs on a connection of its own (lockstep.Follow): it queues
// behind the issue's owner lock before the comment's row, holding no lock on comments while it
// waits at the owner, and never behind the project's row. The anchored_in it states then names
// the block's node under the comment's own comment.anchor_refreshed, which only an append after
// that event can carry.
func TestTheAnchorRefreshTakesTheOwnerRowTheRowTheEventThenTheTuple(t *testing.T) {
	ctx := context.Background()
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "Introduction.\n\nTarget passage.\n")
	commentID := insertAnchoredComment(t, service, artifactID, "Target passage")
	database := service.store
	block, err := service.BlockForQuote(ctx, artifactID, "Target passage")
	if err != nil {
		t.Fatalf("resolve the target block: %v", err)
	}
	var refKey string
	if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `select ref_key from artifacts where id = $1::uuid`, artifactID).Scan(&refKey); err != nil {
			return err
		}
		node, _ := graphstore.CommentFacts(nil, commentID, model.Actor{Kind: "user", ID: "alice"}, time.Now().UTC(), graphstore.NoEvent, "", "", "DOC", nil)
		if err := graphstore.Write(ctx, tx, []graphstore.Node{node}, nil); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			update comments
			set node_kind = $2, node_address = $1::text,
			    anchor = jsonb_set(jsonb_set(anchor, '{block_id}', to_jsonb($3::text), true), '{orphaned}', 'true')
			where id = $1::uuid
		`, commentID, graphstore.KindComment, block)
		return err
	}); err != nil {
		t.Fatalf("give the comment its node and a stale orphaned anchor: %v", err)
	}

	runner := storetest.SingleConnection(t, database)
	settler := newTestServiceOn(t, runner)
	settler.settle = time.Hour
	editLiveTree(t, settler, artifactID, replaceRun("Introduction.", "Intro."))
	settleCtx, cancelSettle := context.WithTimeout(ctx, 10*time.Second)
	defer cancelSettle()
	if err := settler.waitForPendingUpdates(settleCtx, artifactID); err != nil {
		t.Fatalf("wait for live updates to reach persistence: %v", err)
	}
	if err := settler.waitForDurableAppends(settleCtx, artifactID); err != nil {
		t.Fatalf("wait for live updates to become durable: %v", err)
	}
	state := settler.room(artifactID)
	state.mu.Lock()
	generation := state.roomGeneration
	state.mu.Unlock()
	pid := backendOf(t, runner)

	owner := lockstep.HoldOwner(t, database, &lockstep.Hold{
		Name: "issue DOC-1's owner lock", Step: lockstep.StepOwner, Taken: true,
		Statement: lockstep.OwnerLockStatement,
		Unlocked:  []string{"comments"},
	}, store.IssueOwner("DOC-1"))
	row := lockstep.HoldRow(t, database, &lockstep.Hold{
		Name: "the comment's row", Step: lockstep.StepProjection, Taken: true,
		Statement: "update comments as target set anchor = jsonb_set(",
	}, `select 1 from comments where id = $1::uuid for update`, commentID)
	project := lockstep.HoldRow(t, database, &lockstep.Hold{Name: "project DOC's row", Step: lockstep.StepEvent},
		`select 1 from projects where key = $1 for no key update`, "DOC")
	done := make(chan error, 1)
	go func() {
		settler.settleRoom(artifactID, generation)
		done <- nil
	}()
	lockstep.Follow(t, database, "the anchor refresh", pid, done, []*lockstep.Hold{owner, row, project})

	held, err := graphstore.CurrentOut(ctx, database.Pool, graphstore.KindComment, commentID, []string{graphstore.TupleAnchoredIn})
	if err != nil {
		t.Fatalf("read the comment's anchored_in: %v", err)
	}
	var refreshed int64
	if err := database.Pool.QueryRow(ctx, `
		select id from events where type = 'comment.anchor_refreshed' and payload->>'id' = $1 order by id desc limit 1
	`, commentID).Scan(&refreshed); err != nil {
		t.Fatalf("read the comment's comment.anchor_refreshed: %v", err)
	}
	if len(held) != 1 || held[0].ToID != refKey+"#"+block || held[0].SourceEvent != refreshed {
		t.Errorf("the comment states anchored_in %+v, want one to %s#%s caused by its comment.anchor_refreshed %d: the run followed is not one that committed",
			held, refKey, block, refreshed)
	}
}

// backendOf is the backend process id of a one-connection store's connection (storetest.
// SingleConnection), which every statement of a run on it goes through.
func backendOf(t *testing.T, runner *store.Store) uint32 {
	t.Helper()
	var pid uint32
	if err := runner.Pool.QueryRow(context.Background(), `select pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatalf("read the run's backend: %v", err)
	}
	return pid
}
