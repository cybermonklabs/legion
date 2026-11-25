package docs

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// AnchorBlockBackfillActor is who an anchor block backfill records as the writer of each
// anchored_in it states: the command is nobody's act.
var AnchorBlockBackfillActor = model.Actor{Kind: "system", ID: "dispatch-anchor-block-backfill"}

// AnchorBlockBackfill reports how many legacy anchor rows gained their stable block identity.
type AnchorBlockBackfill struct {
	Asks     int
	Comments int
	Skipped  int
}

// BackfillAnchorBlocks resolves legacy quote anchors that still have one matching passage.
// Rows with a missing or ambiguous quote stay unchanged, so the command is idempotent and never
// guesses a block for an old row. A comment's anchor it gives a block states its anchored_in to
// that block in the transaction that writes the block id (pinCommentAnchorBlock).
//
// It writes comment and ask rows while the server serves them, so it takes each row's owner
// lock before it writes that row, as every other serving writer of an item row does
// (refs.LockItemOwner, api's item-writer census): one transaction per row, the lock first and
// the update last (backfillAnchorBlock).
func (s *Service) BackfillAnchorBlocks(ctx context.Context) (AnchorBlockBackfill, error) {
	ctx = store.WithTransactionTracking(ctx)
	var result AnchorBlockBackfill
	for _, target := range []struct {
		table string
		kind  refs.ItemKind
		count *int
	}{
		{table: "asks", kind: refs.ItemAsk, count: &result.Asks},
		{table: "comments", kind: refs.ItemComment, count: &result.Comments},
	} {
		updated, skipped, err := s.backfillAnchorBlocks(ctx, target.table, target.kind)
		if err != nil {
			return AnchorBlockBackfill{}, err
		}
		*target.count = updated
		result.Skipped += skipped
	}
	return result, nil
}

// backfillAnchorBlocks pins the block of every legacy quote anchor of table, a row at a time,
// and answers how many rows it updated and how many it skipped.
func (s *Service) backfillAnchorBlocks(ctx context.Context, table string, kind refs.ItemKind) (int, int, error) {
	candidates, err := s.anchorBlockCandidates(ctx, table)
	if err != nil {
		return 0, 0, err
	}
	updated := 0
	skipped := 0
	for _, id := range candidates {
		rowUpdated, rowSkipped, err := s.backfillAnchorBlock(ctx, table, kind, id)
		if err != nil {
			return 0, 0, err
		}
		if rowUpdated {
			updated++
		}
		if rowSkipped {
			skipped++
		}
	}
	return updated, skipped, nil
}

// anchorBlockCandidates lists the rows of table whose anchor holds a quote and no block, in id
// order, as the rows stood when the command started. It takes no lock and decides nothing: each
// row is read again under its own owner's lock before anything is written of it
// (backfillAnchorBlock). The cursor is this function's whole body, so it is closed before the
// first of those transactions opens, which is what keeps the command to one pooled connection
// at a time (store.ErrNestedAcquire).
func (s *Service) anchorBlockCandidates(ctx context.Context, table string) ([]string, error) {
	rows, err := s.store.Pool.Query(ctx, fmt.Sprintf(`
		select id::text
		from %s
		where anchor is not null and nullif(anchor->>'block_id', '') is null
		order by id
	`, table))
	if err != nil {
		return nil, fmt.Errorf("list %s anchor blocks: %w", table, err)
	}
	defer rows.Close()

	var candidates []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan %s anchor block: %w", table, err)
		}
		candidates = append(candidates, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate %s anchor blocks: %w", table, err)
	}
	return candidates, nil
}

// backfillAnchorBlock pins the block of one legacy quote anchor in a transaction of its own, and
// reports whether it updated the row and whether it skipped it.
//
// The order of the transaction's statements is this command's requirement, not its style, and a
// window test declares it (TestXidWindowAnchorBlockBackfill):
//
//   - the row's owner lock, which every writer of a comment or an ask row takes before it
//     writes that row (refs.LockItemOwner). It is advisory, so it assigns no transaction id;
//   - the re-read of that row's anchor under the lock, so the decision is made on the row as the
//     lock froze it: a row another writer pinned or deleted while this waited for the lock is
//     left alone, and the quote resolved below is the quote the row holds now;
//   - the quote's block, which is document work: it reads the document through this transaction,
//     as a write route's document work reads through its own (Service.Join), and the reads it
//     runs are reads, so the transaction still holds no transaction id when they are done;
//   - the update, the first statement that gives this transaction a transaction id, and for a
//     comment whose row names a node, the anchored_in its pinned block states, a write after it
//     and the only other statement of the window (pinCommentAnchorBlock);
//   - the commit.
//
// The guard the update carries,
//
//	where id = $1 and nullif(anchor->>'block_id', '') is null
//
// is there for idempotence: a second run of the command writes nothing, and neither does a row
// another writer pinned between the re-read and the update. It is not what holds a writer of
// this row off while the quote is resolved - the owner lock above it is - and it does not stand
// in for that lock.
func (s *Service) backfillAnchorBlock(ctx context.Context, table string, kind refs.ItemKind, id string) (bool, bool, error) {
	tx, err := s.store.Pool.Begin(ctx)
	if err != nil {
		return false, false, fmt.Errorf("begin the %s anchor %s backfill: %w", table, id, err)
	}
	defer tx.Rollback(ctx)

	switch found, err := refs.LockItemOwner(ctx, tx, kind, id); {
	case err != nil:
		return false, false, fmt.Errorf("lock the owner of %s anchor %s: %w", table, id, err)
	case !found:
		// The row is gone, before the lock or while this waited for it: it is nobody's to pin.
		return false, false, nil
	}

	var encoded []byte
	switch err := tx.QueryRow(ctx, fmt.Sprintf(`
		select anchor
		from %s
		where id = $1 and anchor is not null and nullif(anchor->>'block_id', '') is null
	`, table), id).Scan(&encoded); {
	case errors.Is(err, pgx.ErrNoRows):
		// Pinned, unanchored or deleted since the listing, so this row is no longer a candidate.
		return false, false, nil
	case err != nil:
		return false, false, fmt.Errorf("read %s anchor %s under its owner: %w", table, id, err)
	}
	var anchor model.Anchor
	if err := model.DecodeAnchor(encoded, &anchor); err != nil {
		return false, false, fmt.Errorf("decode %s anchor block: %w", table, err)
	}

	// The quote is resolved through this transaction, as every document read made while one of
	// this pool's transactions is open is (Service.Join, queryFrom): a document read that took a
	// second pooled connection under the open one is what deadlocks the pool
	// (store.ErrNestedAcquire). The reads it runs here take no transaction id, so the window
	// still opens at the update below. Nothing is written under this ledger, so none of it is
	// queued and there is nothing of it to commit; it is discarded as every joiner discards its
	// own, which hands back whatever live-write handle a read of the document took.
	joined, ledger := s.Join(ctx, tx)
	defer ledger.Discard()
	blockID, err := s.BlockForQuote(joined, anchor.ArtifactID, anchor.Quote)
	var ambiguous *pmdoc.ErrTargetAmbiguous
	switch {
	case errors.Is(err, pmdoc.ErrTargetNotFound), errors.As(err, &ambiguous):
		return false, true, nil
	case err != nil:
		return false, false, fmt.Errorf("resolve %s anchor %s: %w", table, id, err)
	case blockID == "":
		return false, true, nil
	}

	pinned, err := s.pinAnchorBlock(ctx, tx, table, id, blockID)
	if err != nil {
		return false, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, false, fmt.Errorf("commit the %s anchor %s backfill: %w", table, id, err)
	}
	return pinned, false, nil
}

// pinAnchorBlock writes blockID into the anchor of row id of table in tx where that anchor still
// names no block, and answers whether it did. An ask's anchor is written on its own; a comment's
// with its anchored_in (pinCommentAnchorBlock).
func (s *Service) pinAnchorBlock(ctx context.Context, tx pgx.Tx, table, id, blockID string) (bool, error) {
	if table == "comments" {
		return s.pinCommentAnchorBlock(ctx, tx, id, blockID)
	}
	command, err := tx.Exec(ctx, fmt.Sprintf(`
		update %s
		set anchor = jsonb_set(anchor, '{block_id}', to_jsonb($2::text), true)
		where id = $1 and nullif(anchor->>'block_id', '') is null
	`, table), id, blockID)
	if err != nil {
		return false, fmt.Errorf("update %s anchor %s: %w", table, id, err)
	}
	return command.RowsAffected() > 0, nil
}

// pinCommentAnchorBlock writes blockID into the anchor of comment id in tx where that anchor still
// names no block, and states the anchored_in the anchor now names (graphstore.AnchoredIn), in the
// same transaction: the comment's row, which the update locks, then the tuple, dated now by
// AnchorBlockBackfillActor at the sentinel, since the backfill appends no event. The tuple's far
// end is read off the row the update leaves (graphstore.CommentAnchoredInSQL), so an anchor the
// refresh orphaned, or a comment the store holds no node of, states none. tx holds the comment's
// owner lock (backfillAnchorBlock), as a settlement that refreshes the anchors holds its
// document owner's, and it takes no owner row: the refresh writes only the anchor's quote and
// orphan flag, never its block, and the two meet at the lock and the row alone.
func (s *Service) pinCommentAnchorBlock(ctx context.Context, tx pgx.Tx, id, blockID string) (bool, error) {
	var block *string
	err := tx.QueryRow(ctx, `
		update comments c
		set anchor = jsonb_set(c.anchor, '{block_id}', to_jsonb($2::text), true)
		where c.id = $1 and nullif(c.anchor->>'block_id', '') is null and `+graphstore.KeptHeadRows+`
		returning case when c.node_kind is not null then `+graphstore.CommentAnchoredInSQL("c")+` end
	`, id, blockID).Scan(&block)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("update comments anchor %s: %w", id, err)
	}
	if block == nil || !graphstore.AddressFits(*block) {
		return true, nil
	}
	anchored := graphstore.AnchoredIn(id, *block, true, AnchorBlockBackfillActor, time.Now().UTC(), graphstore.NoEvent)
	if err := store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
		graphstore.QueueWrite(b, nil, []graphstore.Tuple{anchored})
		graphstore.QueueHeads(b, graphstore.HeadsOf(nil, []graphstore.Tuple{anchored}))
		return nil
	}); err != nil {
		return false, fmt.Errorf("state comment %s's anchored_in: %w", id, err)
	}
	return true, nil
}
