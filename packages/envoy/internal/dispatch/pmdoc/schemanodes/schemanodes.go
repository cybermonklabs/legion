// Package schemanodes builds the block schema from the nodes of the store, which is where a
// type lives: one `supertag` node per type and one `attribute` node per attribute it holds, tied
// to its supertag by an `attribute_of` tuple
// (store/migrations/20261109051232_graph_supertags.up.sql seeds them from
// pmdoc/schema/blocks.json). The server installs what this reads at boot and again each time a
// supertag write announces itself, so adding a type is a row rather than a build.
//
// Nothing here is on a write's path. A document write validates against the installed value
// (pmdoc), never against the store, so a write costs what it cost before a type was data.
package schemanodes

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

const (
	// KindSupertag is the node kind of a type: its id is the type's name.
	KindSupertag = "supertag"
	// KindAttribute is the node kind of one attribute of one type: its id is
	// `<supertag>/<name>`.
	KindAttribute = "attribute"
	// TupleAttributeOf is the tuple kind tying an attribute node to the supertag that holds it.
	// It is the tie a reader follows; an id's spelling is not.
	TupleAttributeOf = "attribute_of"
	// Channel is the Postgres channel a write to either kind announces itself on at commit
	// (the triggers of 20261109051232). Each server's reinstall waits on its wake.
	Channel = "dispatch_block_schema"
)

// supertagAttributes is what a supertag node carries. A node without BlockType is a type the
// block schema does not declare - a person, a comment, an issue - which this reader skips: the
// block schema is the part of the store a build can address, and a kind it cannot address is
// one more row, never a refusal.
type supertagAttributes struct {
	BlockType bool                   `json:"block_type"`
	Content   pmdoc.BlockContentRule `json:"content"`
	Render    string                 `json:"render"`
	Version   int                    `json:"version"`
	// Position is the type's place in the schema, which is the order the server serves the
	// types in and the order the seed read them from the file.
	Position int `json:"position"`
}

// attributeAttributes is what an attribute node carries: the name its type declares it under,
// its place among that type's attributes, and the schema's own six fields - the kind, the
// choices, the default, whether the server owns it, what it may point at and its scope.
type attributeAttributes struct {
	Name     string `json:"name"`
	Position int    `json:"position"`
	pmdoc.BlockAttributeSchema
}

// Read builds the block schema from the store's nodes: every live supertag node that declares
// itself a block type, with the attributes its `attribute_of` tuples name, in the order each
// node records. The version is the highest its types carry, so a type added at a new version
// raises the schema's own.
//
// It refuses a store holding no block type, since installing that would leave the server
// unable to read a single typed block, and a node it cannot decode. Everything else it cannot
// address it skips.
func Read(ctx context.Context, q graphstore.Queryer) (pmdoc.BlockSchema, error) {
	attributes, err := readAttributeNodes(ctx, q)
	if err != nil {
		return pmdoc.BlockSchema{}, err
	}
	supertags, err := graphstore.NodesOfKind(ctx, q, KindSupertag)
	if err != nil {
		return pmdoc.BlockSchema{}, err
	}
	type placed struct {
		position int
		typ      pmdoc.BlockTypeSchema
	}
	var types []placed
	var value pmdoc.BlockSchema
	for _, node := range supertags {
		if node.State != graphstore.StateLive {
			continue
		}
		var carried supertagAttributes
		if err := decodeNodeAttributes(node, &carried); err != nil {
			return pmdoc.BlockSchema{}, err
		}
		if !carried.BlockType {
			continue
		}
		declared, err := typeAttributes(ctx, q, node.ID, attributes)
		if err != nil {
			return pmdoc.BlockSchema{}, err
		}
		types = append(types, placed{position: carried.Position, typ: pmdoc.BlockTypeSchema{
			Name:       node.ID,
			Content:    carried.Content,
			Render:     carried.Render,
			Attributes: declared,
		}})
		if carried.Version > value.Version {
			value.Version = carried.Version
		}
	}
	if len(types) == 0 {
		return pmdoc.BlockSchema{}, fmt.Errorf("the store holds no block type: no %s node declares one", KindSupertag)
	}
	sort.SliceStable(types, func(left, right int) bool {
		if types[left].position != types[right].position {
			return types[left].position < types[right].position
		}
		return types[left].typ.Name < types[right].typ.Name
	})
	value.Types = make([]pmdoc.BlockTypeSchema, len(types))
	for index, held := range types {
		value.Types[index] = held.typ
	}
	return value, nil
}

// readAttributeNodes answers every live attribute node by its address, so the tuples out of one
// supertag are resolved without a read per attribute.
func readAttributeNodes(ctx context.Context, q graphstore.Queryer) (map[string]attributeAttributes, error) {
	nodes, err := graphstore.NodesOfKind(ctx, q, KindAttribute)
	if err != nil {
		return nil, err
	}
	held := make(map[string]attributeAttributes, len(nodes))
	for _, node := range nodes {
		if node.State != graphstore.StateLive {
			continue
		}
		var carried attributeAttributes
		if err := decodeNodeAttributes(node, &carried); err != nil {
			return nil, err
		}
		held[node.ID] = carried
	}
	return held, nil
}

// typeAttributes is one type's attributes, in the order the nodes record, read through the
// `attribute_of` tuples pointing at its supertag. An attribute node the tuple names and the
// store no longer holds live is left out, as the current set of a retracted tuple would be.
func typeAttributes(ctx context.Context, q graphstore.Queryer, name string, held map[string]attributeAttributes) (pmdoc.BlockAttributes, error) {
	tuples, err := graphstore.CurrentIn(ctx, q, KindSupertag, name, []string{TupleAttributeOf})
	if err != nil {
		return nil, err
	}
	type placed struct {
		position  int
		attribute pmdoc.BlockAttribute
	}
	var attributes []placed
	for _, tuple := range tuples {
		if tuple.FromKind != KindAttribute {
			continue
		}
		carried, live := held[tuple.FromID]
		if !live {
			continue
		}
		attributes = append(attributes, placed{position: carried.Position, attribute: pmdoc.BlockAttribute{
			Name:                 carried.Name,
			BlockAttributeSchema: carried.BlockAttributeSchema,
		}})
	}
	sort.SliceStable(attributes, func(left, right int) bool {
		if attributes[left].position != attributes[right].position {
			return attributes[left].position < attributes[right].position
		}
		return attributes[left].attribute.Name < attributes[right].attribute.Name
	})
	declared := make(pmdoc.BlockAttributes, len(attributes))
	for index, held := range attributes {
		declared[index] = held.attribute
	}
	return declared, nil
}

// decodeNodeAttributes reads a node's attributes column into the shape its kind carries. The
// column is a map on the way out of the store and a record here, so it goes through JSON once.
func decodeNodeAttributes(node graphstore.Node, into any) error {
	encoded, err := json.Marshal(node.Attributes)
	if err != nil {
		return fmt.Errorf("encode the attributes of node %s/%s: %w", node.Kind, node.ID, err)
	}
	if err := json.Unmarshal(encoded, into); err != nil {
		return fmt.Errorf("read the attributes of node %s/%s: %w", node.Kind, node.ID, err)
	}
	return nil
}
