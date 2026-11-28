// Package graphseedtest holds what the tests of other packages share to seed the node and tuple
// store: the rows its writers refuse to write, and the facts a writer of another package states
// beside a node, for a test to write through the store's own writer. Nothing a deployment builds
// imports it. It imports graphstore, so it is a package of its own beside graphstoretest, which
// graphstore's own tests import and which therefore imports nothing of graphstore.
package graphseedtest

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// SeedAuthorOnly reserves each of nodes for its author by writing its row straight into the
// store, in the caller's transaction: the node row at visibility author, with the base
// attributes the store's writer states of a node its insert makes - its visibility and state, and
// its project and parties where it carries them - for a node the store does not hold, and the one
// visibility tuple that move states for a node it holds at another visibility. A node already
// reserved for its author moves nothing and states nothing.
//
// Each node is one statement, which reads whether the store holds the node, moves or makes its
// row and states its facts at once, and the nodes' statements are one batch of their own
// (store.RunWrite), as a write of the store's own runs: no Go code reads what one statement did
// before the next is sent.
//
// The store's writers take a node reserved for its author only of a kind the read registry
// flags (graphstore.AdmitAuthorOnly), and no kind cybermonklabs/legion#3178 registers is flagged,
// since not every read of those kinds binds the visibility predicate. The reads that do bind it
// are still held to answering such a node to its author alone, and a test of them seeds the node
// here, as the rows a writer would have stored, rather than through a writer that refuses it.
func SeedAuthorOnly(ctx context.Context, tx pgx.Tx, nodes ...graphstore.Node) error {
	attributes := graphstore.BaseAttributeValues([...]string{`node.visibility`, `node.state`, `node.project_key`, `node.parties_value`})
	return store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
		for _, node := range nodes {
			args, err := authorOnlyParams(node)
			if err != nil {
				return err
			}
			// The row reserved for its author where the store holds it at another visibility, and
			// its visibility tuple; the row made where the store holds none, and its base
			// attributes, in the order the store's writer states them; nothing where the store
			// holds the row reserved already.
			b.Queue(fmt.Sprintf("seed %s %s reserved for its author", node.Kind, node.ID), `
				with node as (
					select $1::text as kind, $2::text as id, $3::text as supertag, $4::text as visibility, $5::text as state,
					       $6::jsonb as attributes, $7::timestamptz as made_at, $8::jsonb as made_by, $9::bigint as source_event,
					       $10::text as project_key, $11::text[] as parties, $12::text as parties_value
				),
				reserved as (
					update graph_nodes set visibility = node.visibility
					from node
					where graph_nodes.kind = node.kind and graph_nodes.id = node.id and graph_nodes.visibility <> node.visibility
					returning graph_nodes.kind
				),
				made as (
					insert into graph_nodes (kind, id, supertag, visibility, state, attributes, made_at, made_by, source_event, project_key, parties)
					select kind, id, supertag, visibility, state, attributes, made_at, made_by, source_event, project_key, parties
					from node
					on conflict (kind, id) do nothing
					returning kind
				)
				insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, legacy_source_seq, asserted)
				select node.kind, node.id, attribute.kind, $13::text, attribute.value, node.made_at, node.made_by, node.source_event, null, true
				from node
				cross join lateral (values
					`+attributes+`
				) as attribute(bit, ordinal, kind, value)
				where attribute.value is not null
				  and (exists (select 1 from made) or (attribute.ordinal = 0 and exists (select 1 from reserved)))
				order by attribute.ordinal
			`, args...)
		}
		return nil
	})
}

// authorOnlyParams are the thirteen parameters SeedAuthorOnly's statement binds for node, in its
// order: the node's row at visibility author, the value its parties attribute states
// (graphstore.PartiesValue), and the to_kind of a value tuple (graphstore.KindValue).
func authorOnlyParams(node graphstore.Node) ([]any, error) {
	node.Visibility = graphstore.VisibilityAuthor
	madeBy, err := json.Marshal(node.MadeBy.Recorded())
	if err != nil {
		return nil, fmt.Errorf("encode the writer of %s %s: %w", node.Kind, node.ID, err)
	}
	attributes := []byte("{}")
	if node.Attributes != nil {
		if attributes, err = json.Marshal(node.Attributes); err != nil {
			return nil, fmt.Errorf("encode the attributes of %s %s: %w", node.Kind, node.ID, err)
		}
	}
	var project, partiesValue *string
	if node.Project != "" {
		project = &node.Project
	}
	var parties []string
	if len(node.Parties) > 0 {
		parties = node.Parties
		value, err := graphstore.PartiesValue(node.Parties)
		if err != nil {
			return nil, fmt.Errorf("the parties of %s %s: %w", node.Kind, node.ID, err)
		}
		partiesValue = &value
	}
	return []any{node.Kind, node.ID, node.Supertag, node.Visibility, node.State, attributes, node.MadeAt, madeBy,
		node.SourceEvent, project, parties, partiesValue, graphstore.KindValue}, nil
}

// AuthoredFacts is what the reasoning write (api's writeReasoningNodes) states beside node, for a
// test to write with it in one graphstore.Write: each of tuples out of node, asserted and dated,
// signed and caused as the node is, and the writer graphstore.AuthorOf names of node's MadeBy -
// that writer's node, restated so a write of it again keeps what the store holds, and its facts,
// the authored_by among them (graphstore.AuthorFacts). The node itself is the caller's to write
// with them, or to seed where the store's writers refuse it (SeedAuthorOnly). A node whose writer
// names nobody is refused, since the reasoning write states no author of it.
func AuthoredFacts(node graphstore.Node, tuples []graphstore.Tuple) ([]graphstore.Node, []graphstore.Tuple, error) {
	authors, facts, named := graphstore.AuthorFacts(node.Kind, node.ID, node.MadeBy, node.MadeAt, node.SourceEvent)
	if !named {
		return nil, nil, fmt.Errorf("the writer %+v of %s %s names nobody", node.MadeBy, node.Kind, node.ID)
	}
	for index := range authors {
		authors[index].Restates = true
	}
	stated := make([]graphstore.Tuple, 0, len(tuples)+len(facts))
	for _, tuple := range tuples {
		tuple.FromKind, tuple.FromID = node.Kind, node.ID
		tuple.At, tuple.Who, tuple.SourceEvent, tuple.Asserted = node.MadeAt, node.MadeBy, node.SourceEvent, true
		stated = append(stated, tuple)
	}
	return authors, append(stated, facts...), nil
}

// SeedSupertags writes a supertag node for each of names, in the caller's transaction: the type of
// a kind of node only a test writes. The store's writers take a node only of a type the store
// holds (graphstore.ErrSupertagNotHeld), and no migration seeds a type for a test's own kind, so a
// test that writes one seeds its type here first. A name the store holds already is left as it is.
func SeedSupertags(ctx context.Context, tx pgx.Tx, names ...string) error {
	if _, err := tx.Exec(ctx, `
		insert into graph_nodes (kind, id, supertag, visibility, state, attributes, made_at, made_by, source_event)
		select 'supertag', named.id, 'supertag', 'project', 'live', '{}', now(), '{"kind": "system", "id": "graphstore-test"}', 0
		from unnest($1::text[]) as named(id)
		on conflict (kind, id) do nothing`, names); err != nil {
		return fmt.Errorf("seed the supertags %v: %w", names, err)
	}
	return nil
}
