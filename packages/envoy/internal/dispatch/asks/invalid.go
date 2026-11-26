package asks

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc/schemanodes"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// PlanInvalidReasons plans what a settlement states on the node of each ask whose block it read
// the readability of: for each ask of invalid, whose block fails the strict read, the reason it
// fails it for as the node's `invalid` value; and for each ask of readable, whose block reads
// again, the retraction of the value its node holds. It reads, through q, the value each node
// holds now, and states nothing on a node that already holds its ask's reason or holds none for
// a block that reads, so a settlement that moved no block's readability appends nothing. Only an
// ask whose row names its node has one (asks.node_kind): a row the release before wrote is stated
// on the store by the boot step's backfill. It is the one writer of the value beside the one
// writer of the answer (Settle), so the ask node's tuples have one write path (R3).
//
// It answers the step that queues the tuples on a write phase under cause, in who's name at at,
// and nil where there is none to state.
func PlanInvalidReasons(ctx context.Context, q Queryer, invalid map[string]string, readable []string, who model.Actor, at time.Time) (func(b *store.WriteBatch, cause int64) error, error) {
	if len(invalid) == 0 && len(readable) == 0 {
		return nil, nil
	}
	kind := schemanodes.StoreName("ask", "invalid")
	rows, err := q.Query(ctx, `
		select k.id::text, coalesce(held.to_id, ''), coalesce(held.asserted, false)
		from asks k
		left join lateral (
			select t.to_id, t.asserted from graph_tuples t
			where t.from_kind = $2 and t.from_id = k.id::text and t.kind = $3 and t.to_kind = $4
			order by t.seq desc limit 1
		) held on true
		where k.id = any($1::uuid[]) and k.node_kind is not null
		order by k.id
	`, slices.Concat(slices.Sorted(maps.Keys(invalid)), readable), graphstore.KindAsk, kind, graphstore.KindValue)
	if err != nil {
		return nil, fmt.Errorf("read the readability the ask nodes hold: %w", err)
	}
	var tuples []graphstore.Tuple
	for rows.Next() {
		var ask, current string
		var holds bool
		if err := rows.Scan(&ask, &current, &holds); err != nil {
			rows.Close()
			return nil, fmt.Errorf("read the readability an ask node holds: %w", err)
		}
		tuple := graphstore.Tuple{
			FromKind: graphstore.KindAsk, FromID: ask, Kind: kind, ToKind: graphstore.KindValue,
			At: at, Who: who,
		}
		reason, flagged := invalid[ask]
		switch {
		case flagged && (!holds || current != reason):
			tuple.ToID, tuple.Asserted = reason, true
		case !flagged && holds:
			tuple.ToID = current
		default:
			continue
		}
		tuples = append(tuples, tuple)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the readability the ask nodes hold: %w", err)
	}
	if len(tuples) == 0 {
		return nil, nil
	}
	return func(b *store.WriteBatch, cause int64) error {
		for index := range tuples {
			tuples[index].SourceEvent = cause
		}
		graphstore.QueueWrite(b, nil, tuples)
		return nil
	}, nil
}
