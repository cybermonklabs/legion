package refs

import (
	"context"
	"maps"
	"slices"
	"strings"

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
// nodeRegistry (below) is the registry: one entry per node kind, carrying that kind's
// predicate beside the loader that reads its nodes, and every read binds its predicate from
// there rather than writing a test of its own. The two live in one value so a kind cannot be
// registered with a loader and no predicate: such an entry does not build, and one written as
// a literal past the constructors is refused at init, where the kind is named (the three lines
// nodeKind states).
// TestEveryNodeKindAnswersAVisibilityPredicate holds the registry's keys to the node kinds the
// store resolves, in both directions, and holds every read's own SQL to predicates this file
// rendered.

// nodeLoader loads the nodes of one kind whose ids are ids into nodes, for the person or
// session who the read acts for: a loader binds its kind's visibility predicate into its own
// SQL, so a node reserved for another author is one the read never answers. Artifact ids are
// uuids on the from side of an edge and ref_keys on the to side; byRefKey says which the ids
// are.
type nodeLoader func(ctx context.Context, q Queryer, who model.Actor, kind string, byRefKey bool, ids []string, nodes map[[2]string]resolvedNode) error

// nodeKind is one node kind's entry in the registry below: the loader that reads the kind's
// nodes, and the predicate that decides which of them the acting person may see
// (nodeVisibility). The two are one value because a kind that resolves with no predicate is a
// read that answers a node its author reserved.
//
// Three lines keep a kind from reaching a read unguarded, and each one sees what the others
// cannot:
//
//   - The constructors below take the predicate as an argument, so a kind registered with a
//     loader and no predicate is a call with an argument missing and the package does not
//     build. This is the line a kind added the way every kind here was added meets.
//   - init refuses an entry no constructor made: a hand-written literal such as
//     {load: loadIssueNodes} compiles, and its predicate is the zero value nobody chose
//     rather than none at all, so the kind would run on a rule no one wrote. The process does
//     not start instead, naming the kind. A compiler cannot tell that literal from a stated
//     predicate; this can.
//   - TestEveryNodeKindAnswersAVisibilityPredicate holds the registry's keys to the node kinds
//     the store itself resolves. A kind the store resolves that the registry never heard of is
//     in no literal and in no call, so neither of the other two lines can see it.
type nodeKind struct {
	// load reads the kind's nodes. It is nil for a kind no read of the graph resolves
	// (unresolvedKind), and such a kind is no key of nodeKinds.
	load nodeLoader
	// visibility is the kind's rule: which of its nodes a read answers (VisibleKind).
	visibility nodeVisibility
	// stated is true of an entry one of the two constructors made, which is every entry whose
	// predicate someone stated: the zero value of nodeVisibility is a predicate in its own
	// right, the rule most kinds take, so the entry carries whether it was chosen rather than
	// defaulted. init refuses the registry over an entry whose stated is false.
	stated bool
}

// resolvedKind registers a node kind the graph addresses: the loader that reads its nodes, and
// the predicate every read of the kind binds.
func resolvedKind(load nodeLoader, visibility nodeVisibility) nodeKind {
	return nodeKind{load: load, visibility: visibility, stated: true}
}

// unresolvedKind registers a node kind the store holds rows of that no read of the graph
// resolves into a node. It answers a predicate like every other kind and carries no loader.
func unresolvedKind(visibility nodeVisibility) nodeKind {
	return nodeKind{visibility: visibility, stated: true}
}

// nodeRegistry is every node kind this build knows: how each one resolves, and the visibility
// predicate every read of it binds. These are node kinds, the ends of an edge, not edge kinds:
// a node kind a newer build adds to the graph is addressable, listed and counted only once it
// has an entry here. The keys are the kinds graph_edges stores; an ask's, a comment's or a
// message's block is stored as ask_block, comment_block or message_block and resolves to a
// node of kind block, as a document's block does.
//
// It is filled in init rather than in its own declaration because the loaders it holds render,
// through VisibleKind, the predicates this same registry answers: as the initializer of the
// variable, that reference is an initialization cycle the compiler refuses, while at init it
// is what it is at a read - a loader that reads the registry long after it is built.
var nodeRegistry map[string]nodeKind

// nodeKinds is every node kind nodeRegistry resolves, sorted, as the reads that filter
// graph_edges in SQL bind it. BacklinkCounts binds it on its mention arm alone, the one arm
// whose from_kind is a column; every other arm it reads fixes from_kind as a literal this list
// holds, so the filter would drop nothing there. A kind the registry carries no loader for is
// the address of no node a read of the graph answers, so it is none of these.
var nodeKinds []string

func init() {
	nodeRegistry = map[string]nodeKind{
		// The kinds the projection tables hold, whose nodes the graph addresses by the id
		// their own loader reads.
		"issue":         resolvedKind(loadIssueNodes, nodeVisibility{}),
		"artifact":      resolvedKind(loadArtifactNodes, nodeVisibility{fromAddress: artifactStoreAddress}),
		"ask":           resolvedKind(loadAskNodes, nodeVisibility{}),
		"comment":       resolvedKind(loadCommentNodes, nodeVisibility{}),
		"message":       resolvedKind(loadMessageNodes, nodeVisibility{}),
		"component":     resolvedKind(loadComponentNodes, nodeVisibility{}),
		"block":         resolvedKind(loadBlockNodes, nodeVisibility{}),
		"ask_block":     resolvedKind(loadAskBlockNodes, nodeVisibility{}),
		"comment_block": resolvedKind(loadCommentBlockNodes, nodeVisibility{}),
		"message_block": resolvedKind(loadMessageBlockNodes, nodeVisibility{}),
		// The kinds the node and tuple store holds itself (graphstore.KindPerson and its four
		// siblings, loadStoreNodes). None of them carries a `dispatch://` address, so the
		// address rule does not reach them; the four other rules do.
		graphstore.KindPerson:          resolvedKind(loadStoreNodes, nodeVisibility{}),
		graphstore.KindSession:         resolvedKind(loadStoreNodes, nodeVisibility{}),
		graphstore.KindRole:            resolvedKind(loadStoreNodes, nodeVisibility{}),
		graphstore.KindPage:            resolvedKind(loadStoreNodes, nodeVisibility{}),
		graphstore.KindDeliveryAttempt: resolvedKind(loadStoreNodes, nodeVisibility{}),
		// The block schema's own nodes, which a type is (migration 20261109051232). Nothing
		// writes one outside a migration, every one of them is project-visible and no read of
		// the graph resolves one, but they are nodes of the store like any other, so the
		// registry answers for them rather than leaving the one read that lists them
		// (pmdoc/schemanodes, through graphstore.NodesOfKind) with no predicate to bind the
		// day a type is reserved for whoever drafted it.
		"supertag":  unresolvedKind(nodeVisibility{}),
		"attribute": unresolvedKind(nodeVisibility{}),
	}
	for _, kind := range slices.Sorted(maps.Keys(nodeRegistry)) {
		if !nodeRegistry[kind].stated {
			// Nothing of this build runs rather than one read of this kind answering under a
			// rule nobody wrote: the entry was written as a literal instead of through
			// resolvedKind or unresolvedKind, and took the zero predicate by default.
			panic("refs: the node kind " + kind + " is registered with no visibility predicate: " +
				"every entry of nodeRegistry is made by resolvedKind or unresolvedKind, which take one")
		}
	}
	nodeKinds = resolvedNodeKinds()
}

// resolvedNodeKinds is the registry's keys that carry a loader, sorted.
func resolvedNodeKinds() []string {
	kinds := make([]string, 0, len(nodeRegistry))
	for kind, entry := range nodeRegistry {
		if entry.load == nil {
			continue
		}
		kinds = append(kinds, kind)
	}
	slices.Sort(kinds)
	return kinds
}

// nodeVisibility is one node kind's visibility rule, the half of its registry entry that says
// who may see its nodes.
type nodeVisibility struct {
	// fromAddress rewrites the id the from side of an edge carries into the id `graph_nodes`
	// keys the node by, and is nil for every kind that spells the two the same. Only an
	// artifact differs: an edge names it by uuid where it writes one and by ref_key where it
	// is written at, and the store holds it under the ref_key its `dispatch://` address
	// spells.
	fromAddress func(expr string) string
}

// artifactStoreAddress is the ref_key of the artifact whose uuid expr names.
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
// address rewrites the registry declares applied per kind. byRefKey says the ids are the ones
// the to side of an edge carries, which are already the ids the store keys a node by; the from
// side carries an artifact's uuid instead, and that kind's rewrite translates it.
//
// A kind the registry does not hold passes: such an end resolves to no node and the read drops
// its edge anyway (resolveNodes), and refusing here would refuse the whole read for one row a
// newer build wrote.
func VisibleEnd(kindExpr, idExpr string, byRefKey bool, param string) string {
	address := idExpr
	if !byRefKey {
		var rewrites strings.Builder
		for _, kind := range VisibilityKinds() {
			rewrite := nodeRegistry[kind].visibility.fromAddress
			if rewrite == nil {
				continue
			}
			rewrites.WriteString(` when '` + kind + `' then ` + rewrite(idExpr))
		}
		if rewrites.Len() > 0 {
			address = `case ` + kindExpr + rewrites.String() + ` else ` + idExpr + ` end`
		}
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
