package graphstore

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// A component of a project's architecture is a node (R9 of the issue-tree spec): kind and supertag
// component, the supertag the store seeds (20261109051232_graph_supertags.up.sql), addressed
// `<project>/<id>` as graph_edges has always addressed one (ComponentAddress). The architecture
// importer writes it, with its part_of to its parent component and its depends_on to each
// component it depends on, which components.parent and component_depends project; an issue's
// affects names each component the issue holds as its own, which issue_component_members
// projects. part_of is single-valued and depends_on and affects hold sets; each writer appends
// what it moves against what the node holds now and nothing else (SetChanges).
const (
	KindComponent     = "component"
	SupertagComponent = "component"
	TupleDependsOn    = "depends_on"
	TupleAffects      = "affects"
)

// ComponentAddress is the address of component id of project: the node's id in the store and the
// end graph_edges names it by, `<project>/<id>`, a component id being unique within its project.
func ComponentAddress(project, id string) string { return project + "/" + id }

// ComponentNode is a component's node as an import writes it: project-visible, live, in its
// project. An import writes every component it holds again, so the node restates the component
// (Node.Restates): one written again keeps what the store holds of its state and visibility,
// unless it was gone, when it comes back live. Its cause is the writer's to fill, or the sentinel
// where no event recorded the write.
func ComponentNode(project, id string, who model.Actor, at time.Time) Node {
	return objectNode(KindComponent, ComponentAddress(project, id), SupertagComponent, project, nil, who, at)
}

// ComponentGone is the node of component id of project stated gone: the component no source
// projects any more, an import having dropped it or its source having been removed. A component
// imported again later comes back live (ComponentNode).
func ComponentGone(project, id string, who model.Actor, at time.Time) Node {
	node := ComponentNode(project, id, who, at)
	node.State = StateGone
	return node
}

// ComponentRelation is one relation kind from the node fromKind/fromID to the component at the
// address to, or, with asserted false, its end: a component's part_of its parent and depends_on
// another component, out of the component's node, or an issue's affects, out of the issue's. Its
// cause is the writer's to fill, or the sentinel where no event recorded the write.
func ComponentRelation(fromKind, fromID, kind, to string, asserted bool, who model.Actor, at time.Time) Tuple {
	return Tuple{
		FromKind: fromKind, FromID: fromID, Kind: kind,
		ToKind: KindComponent, ToID: to,
		At: at, Who: who, SourceEvent: NoEvent, Asserted: asserted,
	}
}

// ComponentRelations answers, by the address of each component node of addresses, the part_of and
// depends_on tuples it states now to another component: the newest tuple of each relation, while
// it asserts (CurrentTupleRead), read through graph_tuples_current in one statement for every
// node. An import and a source removal compare what they state against it, so each appends only
// what moved.
func ComponentRelations(ctx context.Context, q Queryer, addresses []string) (map[string][]Tuple, error) {
	held := map[string][]Tuple{}
	if len(addresses) == 0 {
		return held, nil
	}
	rows, err := q.Query(ctx, `
		select `+tupleColumns+` from (
			`+CurrentTupleRead(`t.from_id, t.kind, t.to_kind, t.to_id`, tupleColumns,
		`t.from_kind = $1 and t.from_id = any($2::text[]) and t.kind = any($3::text[]) and t.to_kind <> '`+KindValue+`'`)+`
		) newest
		where asserted and to_kind = $1
		order by from_id, kind, to_id
	`, KindComponent, addresses, []string{TuplePartOf, TupleDependsOn})
	if err != nil {
		return nil, fmt.Errorf("read the relations of %d components: %w", len(addresses), err)
	}
	tuples, err := scanTuples(rows)
	if err != nil {
		return nil, err
	}
	for _, tuple := range tuples {
		held[tuple.FromID] = append(held[tuple.FromID], tuple)
	}
	return held, nil
}

// SetChanges is what takes one relation kind out of a node from the far ends held, its current
// tuples, to the far ends want: a retraction of each far end of kind that held names and want does
// not, then an assertion of each far end of want that held does not name, each built by state, so a
// set stated again as it stands appends nothing. A single-valued relation is the set of its one far
// end, or of none.
func SetChanges(held []Tuple, kind string, want []string, state func(to string, asserted bool) Tuple) []Tuple {
	var changes []Tuple
	holds := make(map[string]bool, len(held))
	for _, tuple := range held {
		if tuple.Kind != kind {
			continue
		}
		holds[tuple.ToID] = true
		if !slices.Contains(want, tuple.ToID) {
			changes = append(changes, state(tuple.ToID, false))
		}
	}
	for _, to := range want {
		if !holds[to] {
			holds[to] = true
			changes = append(changes, state(to, true))
		}
	}
	return changes
}
