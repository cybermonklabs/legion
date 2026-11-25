package graphcensus

import (
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// visibilityStateProjections are the census's kinds of the visibility and the state of each node
// of the kinds dispatch://INFRA-1950 adds (projections) - a run, a lens, a finding of one, a risk,
// a constraint and a lens kind - each held to the store's own record of them
// (baseAttributeCensus). A deployment can hold none of these, so none refuses on zero rows.
var visibilityStateProjections = []projection{
	baseAttributeCensus(graphstore.KindInteraction, graphstore.KindInteraction, nil, graphstore.VisibilityAuthor),
	baseAttributeCensus(graphstore.KindLens, graphstore.KindLens, nil, ""),
	baseAttributeCensus("finding", refs.BlockNodeKind, graphstore.FindingIDSQL, ""),
	baseAttributeCensus(graphstore.KindRisk, graphstore.KindRisk, nil, ""),
	baseAttributeCensus(graphstore.KindConstraint, graphstore.KindConstraint, nil, ""),
	baseAttributeCensus(graphstore.KindLensKind, graphstore.KindLensKind, nil, graphstore.VisibilityProject),
}

// baseAttributeCensus is the census of the visibility and the state each node of one kind - a
// run, a lens, a finding, a risk, a constraint or a lens kind - carries, name the kind as the
// report names it and nodeKind the kind the store keys its nodes by, narrowed by narrow over a
// node id where it is not nil (a finding is a block node whose id names its lens). A node's row is
// the projection of its own base-attribute tuples (cybermonklabs/legion#3178's R8), and no table
// projects these kinds, so the store's own record stands on the row side: for each node that
// states either attribute, the newest tuple of each, read as graphstore.CurrentValue reads one,
// against the node row's visibility and state columns. A column with no tuple behind it is a node
// no row projects, a tuple whose node has no row is a row naming a node the store does not hold,
// and a column that differs from its newest tuple is a row and a node that disagree; a node whose
// newest state is gone is held to a row at gone as the other kinds are (presentNodes,
// rowDifferences). The digest names each attribute and carries its value's md5, never a value of
// any other attribute.
//
// fixed is the one visibility R1 gives every node of the kind - author for a run, project for a
// lens kind - or empty for a kind whose nodes take either. The row side states it kept, and the
// node side reads whether the node's column keeps it, so a node at any other visibility is a
// difference even where its tuple says the same.
func baseAttributeCensus(name, nodeKind string, narrow func(id string) string, fixed string) projection {
	// narrowed is narrow's predicate over id, and true where narrow is nil.
	narrowed := func(id string) string {
		if narrow == nil {
			return "true"
		}
		return narrow(id)
	}
	newest := func(attribute string) string {
		return `(select case when v.asserted then v.to_id end from graph_tuples v
				where v.from_kind = '` + nodeKind + `' and v.from_id = keyed.id and v.kind = '` + attribute + `'
				  and v.to_kind = '` + graphstore.KindValue + `'
				order by v.seq desc limit 1)`
	}
	digest := func(visibility, state string) string {
		return `'` + graphstore.AttributeVisibility + `:' || coalesce(md5(` + visibility + `), 'none')
		       || ' ` + graphstore.AttributeState + `:' || coalesce(md5(` + state + `), 'none')`
	}
	rowFixed, nodeFixed, note := "", "", ""
	if fixed != "" {
		rowFixed = ` || ' visibility_fixed:kept'`
		nodeFixed = ` || ' visibility_fixed:' || case when n.visibility = '` + fixed + `' then 'kept' else 'broken' end`
		note = ", and a node at any visibility but " + fixed + ", which R1 fixes for the kind, is a difference"
	}
	return projection{
		name:     name + "_visibility_state",
		nodeKind: nodeKind,
		table:    "graph_tuples",
		note: "no table projects a " + name + ", so the row side is the store's own record of each node's visibility and" +
			" state, the newest tuple of each, against the graph_nodes columns; a column with no tuple, a tuple with no" +
			" node row and a column that differs from its newest tuple are each a difference" + note,
		rows: `
		select '` + name + ` ' || held.id, '` + nodeKind + `', held.id, ` + digest("held.visibility", "held.state") + rowFixed + `
		from (
			select keyed.id, ` + newest(graphstore.AttributeVisibility) + ` as visibility,
			       ` + newest(graphstore.AttributeState) + ` as state
			from (
				select distinct t.from_id as id from graph_tuples t
				where t.from_kind = '` + nodeKind + `' and t.to_kind = '` + graphstore.KindValue + `'
				  and ` + kindIn("t.kind", []string{graphstore.AttributeVisibility, graphstore.AttributeState}) + ` and ` + narrowed("t.from_id") + `
			) keyed
		) held
		where held.state is distinct from '` + graphstore.StateGone + `'`,
		nodes: `
		select n.id, ` + digest("n.visibility", "n.state") + nodeFixed + `
		from graph_nodes n where n.kind = '` + nodeKind + `' and ` + narrowed("n.id"),
		mayBeEmpty: true,
	}
}
