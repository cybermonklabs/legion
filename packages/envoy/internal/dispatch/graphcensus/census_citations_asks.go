package graphcensus

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

const askedOn = "'" + graphstore.TupleAskedOn + "'"
const askBlockKind = "'" + refs.BlockNodeKind + "'"

// The row names the artifact that indexes the block, including the tree artifact for an issue
// room. Use the same address as graphstore.AskPlaces.On, even for the block of a removed ask.
const askBlockAddress = ` from asks k
	left join artifacts document on document.id = k.block_artifact_id
	cross join lateral (select document.ref_key || '#' || nullif(k.block_id, '') as address) wanted`

var askBlockProjection = projection{
	name:     graphstore.TupleAskedOn,
	nodeKind: graphstore.KindAsk,
	table:    "asks.block_artifact_id, asks.block_id",
	note:     "the block address each marked ask row names, against the asked_on tuples its node states now",
	rows: `select 'ask ' || k.id::text, k.node_kind, k.node_address, ` + askedOn + `, ` + askBlockKind + `, wanted.address` + askBlockAddress + `
		where k.node_kind is not null and octet_length(wanted.address) <= ` + addressBound,
	tuples: heldTuples(`t.kind = `+askedOn, graphstore.KindAsk),
	pastBound: `select count(*)` + askBlockAddress + `
		where octet_length(wanted.address) > ` + addressBound,
	unmarked: `select count(*)` + askBlockAddress + `
		where k.node_kind is null and octet_length(wanted.address) <= ` + addressBound,
	emptyAgrees: true,
}

var heldAskedOn = graphstore.CurrentTupleRead(`t.to_kind, t.to_id`, `t.to_kind, t.to_id, t.asserted`,
	`t.from_kind = k.node_kind and t.from_id = k.node_address and t.kind = `+askedOn+` and t.to_kind <> '`+graphstore.KindValue+`'`)

// askBlockMoves is the difference for a single marked row: a missing assertion and any stale
// assertions to retract. A target past the address bound is no target.
var askBlockMoves = `
	with held as (` + heldAskedOn + `)
	select ` + askBlockKind + ` as kind, wanted.address as address, true as asserted
	where octet_length(wanted.address) <= ` + addressBound + `
	  and not exists (select 1 from held where asserted and to_kind = ` + askBlockKind + ` and to_id = wanted.address)
	union all
	select to_kind, to_id, false from held where asserted
	  and (to_kind <> ` + askBlockKind + ` or to_id is distinct from wanted.address
	    or octet_length(wanted.address) > ` + addressBound + `)`

var askBlocksOwed = askBlockAddress + `
	where k.node_kind is not null and exists (` + askBlockMoves + `)`

var owedAskBlocksStatement = `select k.id::text, '', coalesce(k.issue_key, ''), coalesce(k.artifact_id::text, '')` + askBlocksOwed + `
	and k.id > $1::uuid order by k.id limit $2`

// backfillAskedOn runs after the ask pass. It supplies the block pointer for asks already marked
// by the earlier release, without rewriting their rows.
func backfillAskedOn(ctx context.Context, database *store.Store) (KindBackfill, error) {
	done := KindBackfill{Kind: graphstore.TupleAskedOn}
	if err := walkBatches(ctx, database, owedAskBlocksStatement, "asks that owe a block pointer", zeroUUID, scanAskBatchRow, askBatchKey, func(batch []askBatchRow) error {
		rows, tuples, err := writeAskedOnBatch(ctx, database, batch)
		if err != nil {
			return err
		}
		done.Rows += rows
		done.Tuples += tuples
		return nil
	}); err != nil {
		return done, err
	}
	if err := database.Pool.QueryRow(ctx, `select count(*)`+askBlocksOwed).Scan(&done.Remaining); err != nil {
		return done, fmt.Errorf("count asks that owe a block pointer: %w", err)
	}
	done.Complete = done.Remaining == 0
	return done, nil
}

// writeAskedOnBatch locks the owners and reads the differences again before appending tuples.
// The original assertion is dated with the ask. Retractions record the time of the backfill.
func writeAskedOnBatch(ctx context.Context, database *store.Store, batch []askBatchRow) (int64, int64, error) {
	var levelled, stated int64
	err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
		if err := lockAskOwners(ctx, tx, batch); err != nil {
			return err
		}
		ids := make([]string, len(batch))
		for i, ask := range batch {
			ids[i] = ask.askID
		}
		rows, err := tx.Query(ctx, `
			select k.node_kind, k.node_address, k.created_at, move.kind, move.address, move.asserted`+askBlockAddress+`
			cross join lateral (`+askBlockMoves+`) move
			where k.id = any($1::uuid[]) and k.node_kind is not null
			order by k.id, move.asserted, move.kind, move.address`, ids)
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		tuples, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (graphstore.Tuple, error) {
			tuple := graphstore.Tuple{Kind: graphstore.TupleAskedOn, Who: backfillActor, SourceEvent: graphstore.NoEvent}
			err := row.Scan(&tuple.FromKind, &tuple.FromID, &tuple.At, &tuple.ToKind, &tuple.ToID, &tuple.Asserted)
			if !tuple.Asserted {
				tuple.At = now
			}
			return tuple, err
		})
		if err != nil || len(tuples) == 0 {
			return err
		}
		heads := graphstore.HeadsOf(nil, tuples)
		if err := store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
			graphstore.QueueWrite(b, nil, tuples)
			graphstore.QueueHeads(b, heads)
			return nil
		}); err != nil {
			return err
		}
		levelled, stated = int64(len(heads)), int64(len(tuples))
		return nil
	})
	if err != nil {
		return 0, 0, fmt.Errorf("write a batch of asked_on tuples: %w", err)
	}
	return levelled, stated, nil
}
