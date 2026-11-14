package refs

import (
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
// nodeRegistry (graph.go) is the registry: one entry per node kind, carrying that kind's
// predicate beside the loader that reads its nodes, and every read binds its predicate from
// there rather than writing a test of its own. The two live in one value so a kind cannot be
// registered with a loader and no predicate: such an entry does not build, and one written as
// a literal past the constructors is refused at init, where the kind is named (graph.go, the
// three lines nodeKind states).
// TestEveryNodeKindAnswersAVisibilityPredicate holds the registry's keys to the node kinds the
// store resolves, in both directions, and holds every read's own SQL to predicates this file
// rendered.

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

// VisibilityArg is the one argument every rendered predicate binds: the acting credential, as
// graphstore's one author rule compares it. A read binds it once and names its placeholder in
// each predicate it renders.
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
