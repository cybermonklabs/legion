// Package graphstoretest holds what the tests of other packages share to seed the node and tuple
// store: the rows its writers refuse to write, and the facts a writer of another package states
// beside a node, for a test to write through the store's own writer. Nothing a deployment builds
// imports it.
package graphstoretest

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
)

// SeedAuthorOnly reserves each of nodes for its author by writing its row straight into the
// store, in the caller's transaction: the node row at visibility author, with the base
// attributes the store's writer states of a node its insert makes (madeFacts) for a node the
// store does not hold, and the one visibility tuple that move states for a node it holds at
// another visibility. A node already reserved for its author moves nothing and states nothing.
//
// The store's writers take a node reserved for its author only of a kind the read registry
// flags (graphstore.AdmitAuthorOnly), and no kind dispatch://INFRA-1149 registers is flagged,
// since not every read of those kinds binds the visibility predicate. The reads that do bind it
// are still held to answering such a node to its author alone, and a test of them seeds the node
// here, as the rows a writer would have stored, rather than through a writer that refuses it.
func SeedAuthorOnly(ctx context.Context, tx pgx.Tx, nodes ...graphstore.Node) error {
	for _, node := range nodes {
		node.Visibility = graphstore.VisibilityAuthor
		madeBy, err := json.Marshal(node.MadeBy.Recorded())
		if err != nil {
			return fmt.Errorf("encode the writer of %s %s: %w", node.Kind, node.ID, err)
		}
		var held bool
		if err := tx.QueryRow(ctx, `select exists (select 1 from graph_nodes where kind = $1 and id = $2)`,
			node.Kind, node.ID).Scan(&held); err != nil {
			return fmt.Errorf("read whether the store holds %s %s: %w", node.Kind, node.ID, err)
		}
		var facts []graphstore.Tuple
		if held {
			tag, err := tx.Exec(ctx, `update graph_nodes set visibility = $3 where kind = $1 and id = $2 and visibility <> $3`,
				node.Kind, node.ID, node.Visibility)
			if err != nil {
				return fmt.Errorf("reserve %s %s for its author: %w", node.Kind, node.ID, err)
			}
			if tag.RowsAffected() == 0 {
				continue
			}
			facts = []graphstore.Tuple{{
				FromKind: node.Kind, FromID: node.ID, Kind: graphstore.AttributeVisibility,
				ToKind: graphstore.KindValue, ToID: node.Visibility,
				At: node.MadeAt, Who: node.MadeBy, SourceEvent: node.SourceEvent, Asserted: true,
			}}
		} else {
			attributes := []byte("{}")
			if node.Attributes != nil {
				if attributes, err = json.Marshal(node.Attributes); err != nil {
					return fmt.Errorf("encode the attributes of %s %s: %w", node.Kind, node.ID, err)
				}
			}
			var project *string
			if node.Project != "" {
				project = &node.Project
			}
			var parties []string
			if len(node.Parties) > 0 {
				parties = node.Parties
			}
			if _, err := tx.Exec(ctx, `
				insert into graph_nodes (kind, id, supertag, visibility, state, attributes, made_at, made_by, source_event, project_key, parties)
				values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
			`, node.Kind, node.ID, node.Supertag, node.Visibility, node.State, attributes, node.MadeAt, madeBy,
				node.SourceEvent, project, parties); err != nil {
				return fmt.Errorf("write the row of %s %s: %w", node.Kind, node.ID, err)
			}
			if facts, err = madeFacts(node); err != nil {
				return err
			}
		}
		for _, fact := range facts {
			who, err := json.Marshal(fact.Who.Recorded())
			if err != nil {
				return fmt.Errorf("encode the writer of %s %s's %s: %w", node.Kind, node.ID, fact.Kind, err)
			}
			if _, err := tx.Exec(ctx, `
				insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, legacy_source_seq, asserted)
				values ($1, $2, $3, $4, $5, $6, $7, $8, null, $9)
			`, fact.FromKind, fact.FromID, fact.Kind, fact.ToKind, fact.ToID, fact.At, who, fact.SourceEvent, fact.Asserted); err != nil {
				return fmt.Errorf("state %s %s's %s: %w", node.Kind, node.ID, fact.Kind, err)
			}
		}
	}
	return nil
}

// madeFacts is the base attributes the store's writer states of a node its insert makes: its
// visibility and state, and its project and parties where it carries them, each dated, signed
// and caused as the node is.
func madeFacts(node graphstore.Node) ([]graphstore.Tuple, error) {
	parties, err := graphstore.PartiesValue(node.Parties)
	if err != nil {
		return nil, fmt.Errorf("the parties of %s %s: %w", node.Kind, node.ID, err)
	}
	values := [...][2]string{
		{graphstore.AttributeVisibility, node.Visibility}, {graphstore.AttributeState, node.State},
		{graphstore.AttributeProject, node.Project}, {graphstore.AttributeParties, parties},
	}
	facts := make([]graphstore.Tuple, 0, len(values))
	for _, value := range values {
		if value[1] == "" {
			continue
		}
		facts = append(facts, graphstore.Tuple{
			FromKind: node.Kind, FromID: node.ID, Kind: value[0], ToKind: graphstore.KindValue, ToID: value[1],
			At: node.MadeAt, Who: node.MadeBy, SourceEvent: node.SourceEvent, Asserted: true,
		})
	}
	return facts, nil
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
