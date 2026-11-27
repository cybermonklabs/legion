package refs

import (
	"context"
	"maps"
	"slices"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// Every read of the graph answers only the nodes the acting person may see. A node carries that
// in `graph_nodes.visibility`: `project` is every node Dispatch writes today, which everyone who
// may read the project reads, and `author` reserves the node for whoever wrote it. A node
// reserved for its author is absent from the list of its kind, from the edges of every other
// node, from the backlink counts, from search, and from a read of its own `dispatch://` address,
// which answers exactly what an address nobody holds answers.
//
// The test is bound into the SQL of each read and never applied in Go, so a count and the list
// behind it cannot disagree: a row the count includes is a row the list shows.
//
// nodeRegistry (below) is the registry: one entry per node kind, the loader that reads its
// nodes, and every read binds its predicate through VisibleKind, VisibleEnd or VisibleKindRow
// rather than writing a test of its own. Every kind answers the same rule - the store's row for
// the node says whether the acting person may see it (visibleWhere) - so an entry carries no
// rule of its own: being a key of the registry is what makes a read of the kind bind that rule
// rather than answer false. The one thing kinds differ in is how the from side of an edge spells
// an artifact, which VisibleEnd rewrites (artifactStoreAddress).
// TestEveryNodeKindAnswersAVisibilityPredicate holds the registry's keys to the node kinds the
// store resolves, in both directions, and holds every read's own SQL to predicates this file
// rendered.

// nodeLoader loads the nodes of one kind whose ids are ids into nodes, for the person or
// session who the read acts for: a loader binds its kind's visibility predicate into its own
// SQL, so a node reserved for another author is one the read never answers. Artifact ids are
// uuids on the from side of an edge and ref_keys on the to side; byRefKey says which the ids
// are.
//
// The block, ask, comment and message loaders join issues, artifacts and artifact_versions by
// lateral probes by key that each stop at their first row, so however many ids a read passes, the
// planner cannot hash-join a sequential read of a whole table, as it did for a plain join to
// issues from about 80 ids with 12,000 issues. Each of those loaders names its probes.
type nodeLoader func(ctx context.Context, q Queryer, who model.Actor, kind string, byRefKey bool, ids []string, nodes map[[2]string]resolvedNode) error

// nodeRegistry is every node kind this build knows, each with the loader that reads its nodes,
// nil for a kind the store holds rows of that no read of the graph resolves into a node. These
// are node kinds, the ends of an edge, not edge kinds: a node kind a newer build adds to the
// graph is addressable, listed and counted only once it has an entry here. The keys are the
// kinds graph_edges stores; an ask's, a comment's or a message's block is stored as ask_block,
// comment_block or message_block and resolves to a node of kind block, as a document's block
// does.
//
// It is filled in init rather than in its own declaration because the loaders it holds render,
// through VisibleKind, the predicates this same registry answers: as the initializer of the
// variable, that reference is an initialization cycle the compiler refuses, while at init it
// is what it is at a read - a loader that reads the registry long after it is built.
var nodeRegistry map[string]nodeLoader

// nodeKinds is every node kind nodeRegistry resolves, sorted, as the reads that filter
// graph_edges in SQL bind it. BacklinkCounts binds it on its mention arm alone, the one arm
// whose from_kind is a column; every other arm it reads fixes from_kind as a literal this list
// holds, so the filter would drop nothing there. A kind the registry carries no loader for is
// the address of no node a read of the graph answers, so it is none of these.
var nodeKinds []string

func init() {
	nodeRegistry = map[string]nodeLoader{
		// The kinds the projection tables hold, whose nodes the graph addresses by the id
		// their own loader reads.
		"issue":         loadIssueNodes,
		"artifact":      loadArtifactNodes,
		"ask":           loadAskNodes,
		"comment":       loadCommentNodes,
		"message":       loadMessageNodes,
		"component":     loadComponentNodes,
		"block":         loadBlockNodes,
		"ask_block":     loadAskBlockNodes,
		"comment_block": loadCommentBlockNodes,
		"message_block": loadMessageBlockNodes,
		// The kinds the node and tuple store holds itself (graphstore.KindPerson and its four
		// siblings, loadStoreNodes). None of them carries a `dispatch://` address, so the
		// artifact's address rewrite does not reach them.
		graphstore.KindPerson:          loadStoreNodes,
		graphstore.KindSession:         loadStoreNodes,
		graphstore.KindRole:            loadStoreNodes,
		graphstore.KindPage:            loadStoreNodes,
		graphstore.KindDeliveryAttempt: loadStoreNodes,
		// The block schema's own nodes, which a type is (migration 20261109051232). Nothing
		// writes one outside a migration, every one of them is project-visible and no read of
		// the graph resolves one, so neither has a loader; but they are nodes of the store like
		// any other, so the registry holds them rather than leaving the one read that lists them
		// (pmdoc/schemanodes, through graphstore.NodesOfKind) with no predicate to bind the day
		// a type is reserved for whoever drafted it.
		"supertag":  nil,
		"attribute": nil,
	}
	nodeKinds = resolvedNodeKinds()
}

// resolvedNodeKinds is the registry's keys that carry a loader, sorted.
func resolvedNodeKinds() []string {
	kinds := make([]string, 0, len(nodeRegistry))
	for kind, load := range nodeRegistry {
		if load == nil {
			continue
		}
		kinds = append(kinds, kind)
	}
	slices.Sort(kinds)
	return kinds
}

// artifactStoreAddress is the ref_key of the artifact whose uuid expr names: the one rewrite of
// the id the from side of an edge carries into the id `graph_nodes` keys the node by, which every
// other kind spells the same. Only an artifact differs: an edge names it by uuid where it writes
// one and by ref_key where it is written at, and the store holds it under the ref_key its
// `dispatch://` address spells.
func artifactStoreAddress(expr string) string {
	return `(select store_artifact.ref_key from artifacts store_artifact where store_artifact.id::text = ` + expr + `)`
}

// VisibilityMarker opens every predicate the registry renders and names the kinds that
// predicate covers. It is what proves a read binds the registry rather than writing its own
// test: a statement whose `graph_nodes` reads are all marked is a statement whose visibility
// rules all came from here (TestEveryNodeKindAnswersAVisibilityPredicate).
const VisibilityMarker = "/* visibility predicate: "

// VisibilityKinds is every node kind the registry holds a predicate for, sorted.
func VisibilityKinds() []string {
	return slices.Sorted(maps.Keys(nodeRegistry))
}

// VisibilityArg is the one argument every rendered predicate binds: the author the acting
// credential proved, as graphstore's one author rule compares it. A read binds it once and names
// its placeholder in each predicate it renders.
func VisibilityArg(who model.Actor) any {
	return graphstore.ActingCredential(who)
}

// VisibleKind is the predicate of one node kind: the SQL that is true of a node of that kind
// the acting person may see. address is the SQL naming the node's id as `graph_nodes` keys it,
// a column of the read's own query, and param the placeholder the acting credential is bound at
// (VisibilityArg).
//
// A node the store holds no row for is visible: `graph_nodes` is the store's own list and the
// projections are still the ones most reads walk, so a node no write has recorded there is as
// project-visible as every node Dispatch wrote before this store existed.
//
// A kind the registry does not hold answers false, so a read that named one lists nothing
// rather than everything: a node kind a newer build adds is addressable, listed and counted
// only once it has an entry here, exactly as that entry's loader decides whether the kind
// resolves at all.
// TestEveryNodeKindAnswersAVisibilityPredicate is what keeps that branch unreachable.
func VisibleKind(kind, address, param string) string {
	if _, held := nodeRegistry[kind]; !held {
		return "false"
	}
	return VisibilityMarker + kind + ` */` + visibleWhere(`'`+kind+`'`, address, param)
}

// VisibleEnd is the predicate over an end of an edge, whose node kind is a column rather than a
// literal: one read of `graph_nodes` keyed on that column and on the id beside it, with the
// artifact's address rewrite (artifactStoreAddress) applied where the column says artifact.
// byRefKey says the ids are the ones the to side of an edge carries, which are already the ids
// the store keys a node by; the from side carries an artifact's uuid instead, and the rewrite
// translates it.
//
// A kind the registry does not hold passes: such an end resolves to no node and the read drops
// its edge anyway (resolveNodes), and refusing here would refuse the whole read for one row a
// newer build wrote.
func VisibleEnd(kindExpr, idExpr string, byRefKey bool, param string) string {
	address := idExpr
	if !byRefKey {
		address = `case ` + kindExpr + ` when 'artifact' then ` + artifactStoreAddress(idExpr) + ` else ` + idExpr + ` end`
	}
	return VisibilityMarker + `every kind */` + visibleWhere(kindExpr, address, param)
}

// VisibleRow is the predicate over a `graph_nodes` row a read already holds, for the one read
// that walks the store's own table (loadStoreNodes): the same test, with no second read of the
// table to make it.
func VisibleRow(alias, param string) string {
	return VisibilityMarker + `the row's own kind */` + visibleHeldRow(alias, param)
}

// VisibleKindRow is one node kind's predicate over a `graph_nodes` row a read already holds:
// the test VisibleKind makes, over the row a read's own join or `exists` has already found, so
// a read that must read the node's row anyway makes both tests out of one read of it - the
// count's tuple arms, which answer no edge of a node whose state is gone, are that read
// (backlink_counts.go's liveVisibleNode). A kind the registry does not hold answers false,
// exactly as VisibleKind refuses it, so a read over a kind with no predicate counts nothing
// rather than everything.
//
// The rule VisibleKind states for a node the store holds no row for - that it is visible - is
// the one difference, and it is the caller's to keep: a test over a row is a test of a row that
// is there, and a read binds this only where it answers nothing for a node that has none.
func VisibleKindRow(kind, alias, param string) string {
	if _, held := nodeRegistry[kind]; !held {
		return "false"
	}
	return VisibilityMarker + kind + ` */` + visibleHeldRow(alias, param)
}

// visibleHeldRow is the body both of those render: the row itself says whether the acting
// person may see the node, with no read of the table to find it.
func visibleHeldRow(alias, param string) string {
	return `(` + alias + `.visibility <> '` + graphstore.VisibilityAuthor +
		`' or ` + graphstore.AuthorIsSQL(alias+`.made_by`, param) + `)`
}

// visibleWhere is the body every rendered predicate shares: the store's row for the node, if it
// holds one, says whether the acting person may see it.
//
// It is a scalar subquery and not an `exists`, so the planner keeps it a sub-plan over the
// `graph_nodes` primary key rather than flattening it into an anti-join that reads the whole
// table: every read that binds this is keyed on an index of its own, and a count that gained a
// sequential scan of the store would be paid on every list.
func visibleWhere(kindExpr, address, param string) string {
	return `coalesce((select visible_node.visibility <> '` + graphstore.VisibilityAuthor +
		`' or ` + graphstore.AuthorIsSQL(`visible_node.made_by`, param) + `
		from graph_nodes visible_node where visible_node.kind = ` + kindExpr + ` and visible_node.id = ` + address + `), true)`
}
