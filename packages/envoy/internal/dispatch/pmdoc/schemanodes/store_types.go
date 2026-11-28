package schemanodes

import (
	"context"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// StoreType is one type of the store that declares no block type - an issue, a comment, a lens, a
// lens kind, a constraint - with the attributes its `attribute_of` tuples name, in the order its
// attribute nodes record. The block schema Read builds reads past such a type, since no document
// holds a block of it, so this is how a reader learns what the store's own types hold.
type StoreType struct {
	Name       string
	Attributes []StoreAttribute
}

// StoreAttribute is one attribute of a StoreType: the name its node carries and the kind of value
// it holds.
type StoreAttribute struct {
	Name string
	Kind pmdoc.BlockAttributeKind
}

// StoreTypesIn reads every supertag node the store holds at any state but gone that declares no
// block type, by name, each with its attributes, in the caller's transaction, tx, which the caller
// opens read-only at repeatable read as Read opens its own, so a migration that commits between
// its reads is read whole or not at all, and a read beside it - api's schema read of the lens
// catalogue - reads the same snapshot. It is read from the store on each call, so a type written
// later is answered by the next call of every process. Its three reads - the attribute nodes, the
// supertag nodes and the `attribute_of` tuples tying the first to the second - go to the store as
// one batch, in one round trip, and each attribute is placed as the block schema places a block
// type's (orderedAttributes).
func StoreTypesIn(ctx context.Context, tx pgx.Tx) ([]StoreType, error) {
	var attributeNodes, supertags []graphstore.Node
	var ties map[string][]string
	batch := &pgx.Batch{}
	graphstore.QueueNodesOfKind(batch, KindAttribute, &attributeNodes)
	graphstore.QueueNodesOfKind(batch, KindSupertag, &supertags)
	queueAttributeTies(batch, &ties)
	if err := tx.SendBatch(ctx, batch).Close(); err != nil {
		return nil, err
	}
	attributes, err := heldAttributes(attributeNodes)
	if err != nil {
		return nil, err
	}
	var types []StoreType
	for _, node := range supertags {
		if node.State == graphstore.StateGone {
			continue
		}
		var carried supertagAttributes
		if err := decodeNodeAttributes(node, &carried); err != nil {
			return nil, err
		}
		if carried.BlockType {
			continue
		}
		held := orderedAttributes(ties[node.ID], attributes)
		typ := StoreType{Name: node.ID, Attributes: make([]StoreAttribute, len(held))}
		for index, attribute := range held {
			typ.Attributes[index] = StoreAttribute{Name: attribute.Name, Kind: attribute.Kind}
		}
		types = append(types, typ)
	}
	sort.Slice(types, func(left, right int) bool { return types[left].Name < types[right].Name })
	return types, nil
}

// queueAttributeTies queues on batch the read of, for each supertag, the attribute nodes its
// current `attribute_of` tuples come from, in one read over graph_tuples_as_of_in for every
// supertag at once, and sets ties to it once the batch's results are read.
func queueAttributeTies(batch *pgx.Batch, ties *map[string][]string) {
	batch.Queue(`
		select newest.to_id, newest.from_id from (
			`+graphstore.CurrentTupleRead(`t.to_id, t.from_kind, t.from_id`, `t.to_id, t.from_id, t.asserted`,
		`t.to_kind = $1 and t.kind = $2 and t.from_kind = $3 and t.to_kind <> '`+graphstore.KindValue+`'`)+`
		) newest
		where newest.asserted`, KindSupertag, TupleAttributeOf, KindAttribute).Query(func(rows pgx.Rows) error {
		read := map[string][]string{}
		for rows.Next() {
			var supertag, attribute string
			if err := rows.Scan(&supertag, &attribute); err != nil {
				return fmt.Errorf("read the attributes tied to each type: %w", err)
			}
			read[supertag] = append(read[supertag], attribute)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("read the attributes tied to each type: %w", err)
		}
		*ties = read
		return nil
	})
}
