package graphcensus

import (
	"context"
	"fmt"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// endedChecks is an SQL array of the ended checks a risk asserted a prevented_by at: each far end of
// such a tuple whose block the store holds gone, which a version dropped, or whose document is
// retired (artifacts.retired_at), whose checks nobody runs. A block's node id names the artifact
// that keys its row by that artifact's ref_key before its first '#', since a ref_key holds none:
// the block's document, or for a block of an issue room the room's tree artifact, and the block's
// document is then the one whose fragment holds it (refs.RoomBlockDocumentSQL). So a check of a
// converted document is judged by that document, as its retirement and the store's rule judge it
// (docs' retiredPreventions, graphstore's pointerRules), and never by the tree, which stands.
// narrow is a predicate over p, the prevented_by tuple, that bounds the checks read. It starts from
// the risks, through graph_tuples_current, since risks are few and a read into blocks would walk
// every block's tuples.
func endedChecks(narrow string) string {
	return `array(
		select distinct p.to_id from graph_tuples p
		left join graph_nodes b on b.kind = p.to_kind and b.id = p.to_id
		left join artifacts keyed on keyed.ref_key = split_part(p.to_id, '#', 1)
		left join artifacts a on a.id = coalesce(` + refs.RoomBlockDocumentSQL("keyed.id", "substr(p.to_id, length(keyed.ref_key) + 2)") + `, keyed.id)
		where p.from_kind = '` + graphstore.KindRisk + `' and p.kind = '` + graphstore.TuplePreventedBy + `'
		  and p.to_kind = '` + refs.BlockNodeKind + `' and p.asserted
		  and (b.state = '` + graphstore.StateGone + `' or a.retired_at is not null)
		  and ` + narrow + `)`
}

// backfillEndedPreventions retracts every prevented_by the store asserts now at an ended check
// (endedChecks), through the read the index pass and the retirement retract by
// (refs.PreventionRetractions). It reads the documents that hold such a check in batches by
// ref_key and retracts each batch's in a transaction of its own (writeEndedPreventions). Rows and
// Tuples count the tuples it retracted.
func backfillEndedPreventions(ctx context.Context, database *store.Store) (KindBackfill, error) {
	done := KindBackfill{Kind: "prevented_by_retracted"}
	err := owedBatches(ctx, database, endedPreventionDocumentsRead, "the documents holding a check nobody runs that a risk is prevented by", "",
		func(owed []string) error {
			retracted, err := writeEndedPreventions(ctx, database, owed)
			if err != nil {
				return err
			}
			done.Rows += retracted
			done.Tuples += retracted
			return nil
		})
	if err != nil {
		return done, err
	}
	if err := database.Pool.QueryRow(ctx, refs.CurrentPreventionsSQL(`count(*)`, endedChecks(`true`))).Scan(&done.Remaining); err != nil {
		return done, fmt.Errorf("count the prevented_by tuples at checks nobody runs: %w", err)
	}
	done.Complete = done.Remaining == 0
	return done, nil
}

// endedPreventionDocumentsRead reads the ref_keys past $1, in order, of the documents that hold
// an ended check a risk is prevented by now, at most $2 of them (owedBatches).
var endedPreventionDocumentsRead = `
		` + refs.CurrentPreventionsSQL(`distinct split_part(checked.to_id, '#', 1)`, endedChecks(`split_part(p.to_id, '#', 1) > $1`)) + `
		order by 1
		limit $2`

// writeEndedPreventions retracts, in one transaction, every prevented_by the store asserts now at an
// ended check of documents, a batch's ref_keys, and answers how many. It takes the owner lock
// (store.LockOwner) of each of the documents that has a row first, each once and in ascending
// owner key (lockDocumentOwners): the lock the index pass that drops a check and the retirement of
// its document hold while they retract, and which api's lockReasoningOwners takes before it
// appends a prevented_by at a check, so such a write beside the pass commits first and the read
// under the lock sees it. It then reads the tuples again under that lock and retracts each, dated
// now and made by the run (backfillActor), caused by no event, as writeGoneRun moves a run's node.
func writeEndedPreventions(ctx context.Context, database *store.Store, documents []string) (int64, error) {
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin the retraction of the prevented_by tuples at checks nobody runs: %w", err)
	}
	defer tx.Rollback(ctx)
	if err := lockDocumentOwners(ctx, tx, `ref_key = any($1)`, documents); err != nil {
		return 0, err
	}
	retracted, err := refs.PreventionRetractions(ctx, tx, endedChecks(`split_part(p.to_id, '#', 1) = any($1)`),
		[]any{documents}, time.Now().UTC(), backfillActor)
	if err != nil {
		return 0, fmt.Errorf("read the prevented_by tuples at checks nobody runs: %w", err)
	}
	if len(retracted) == 0 {
		return 0, nil
	}
	if err := store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
		graphstore.QueueWrite(b, nil, retracted)
		graphstore.QueueHeads(b, graphstore.HeadsOf(nil, retracted))
		return nil
	}); err != nil {
		return 0, fmt.Errorf("retract the prevented_by tuples at checks nobody runs: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit the retraction of the prevented_by tuples at checks nobody runs: %w", err)
	}
	return int64(len(retracted)), nil
}
