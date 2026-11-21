package refs

import (
	"slices"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
)

// Catalogue nodes (the private-projects spec, v34): a node the system seeds for every project
// to use, which holds no project's content, is visible to every viewer - a restricted member, a
// credential that lists projects and the closed viewer included. The clause decides which nodes
// those are by the closed list below and never by guessing from a missing project: a node of a
// catalogue kind that some project wrote is decided by that project's rule.
//
// Each entry compiles to one arm, ORed ahead of its kind's own project and party rule
// (nodeVisibility.scope), and it replaces only that rule: the author rule every kind shares still
// ANDs with it. The entries reach a read only once the registry holds their kind (nodeRegistry),
// which the change that registers the node-types spec's kinds adds; until then a kind the registry
// does not hold answers no node at all.

// catalogueEntry is one kind of catalogue node. Via is nil when every node of Kind with no project
// and no parties is catalogue; otherwise only a node that is the far end of a current Via.Relation
// tuple from a node of Via.FromKind is. Source is the requirement that makes the kind catalogue.
type catalogueEntry struct {
	Kind   string
	Via    *catalogueVia
	Source string
}

// catalogueVia is the one relation that makes a node of a catalogue entry's kind catalogue.
type catalogueVia struct {
	Relation, FromKind string
}

// catalogue is the closed list. The node-types spec's R7: every lens_kind node is catalogue, and
// each one carries a constrained_by tuple to the constraint node that holds its primary source. A
// constraint is also project content - a requirement may point at a constraint it came from (the
// node-types spec's R9) - so a constraint is catalogue only through that tuple, never by its kind
// or its address. The kind names are those of the root's closed supertag list
// (cmd/dispatch/supertag_nodes_test.go:authorisedSupertags), until the node-types work lands its
// graphstore constants.
var catalogue = []catalogueEntry{
	{Kind: "lens_kind", Source: "the node-types spec's R7"},
	{Kind: "constraint", Via: &catalogueVia{Relation: "constrained_by", FromKind: "lens_kind"}, Source: "the node-types spec's R7"},
}

// catalogueEntryOf is the catalogue entry of kind, if the list holds one.
func catalogueEntryOf(kind string) (catalogueEntry, bool) {
	if i := slices.IndexFunc(catalogue, func(entry catalogueEntry) bool { return entry.Kind == kind }); i >= 0 {
		return catalogue[i], true
	}
	return catalogueEntry{}, false
}

// overRow is the entry's arm over a node's own row in graph_nodes, row its alias: the node names no
// project and no parties, and, for an entry with a Via, a current tuple of that relation reaches it
// from a node of Via.FromKind. The tuple is read by the one current-tuple rule
// (graphstore.CurrentTupleRead) through graph_tuples_as_of_in (graph_tuples_inbound until
// #3178's spec v50 R2 change), whose columns lead with the far end and the relation, every one of
// them a literal or the row's own id here; the kind stays a literal, since a parameter would leave
// neither partial index usable. The `exists` sits under
// an OR, so it ends in `offset 0`: without that fence Postgres may plan it as a hashed read of the
// whole log once per statement rather than an index probe per row (#3178 at 0f8b389c).
func (entry catalogueEntry) overRow(row string) string {
	arm := row + `.project_key is null and ` + row + `.parties is null`
	if entry.Via != nil {
		arm += ` and exists (
			select 1 from (` + graphstore.CurrentTupleRead(`t.from_kind, t.from_id`, `t.asserted`,
			`t.to_kind = '`+entry.Kind+`' and t.to_id = `+row+`.id and t.kind = '`+entry.Via.Relation+
				`' and t.from_kind = '`+entry.Via.FromKind+`'`) + `) via
			where via.asserted
			offset 0)`
	}
	return `(` + arm + `)`
}

// holdsWithoutRow reports whether the entry's arm holds for a node the store holds no row of: an
// entry with no Via decides by the kind alone, so a reference to a node the seed has not written
// yet is catalogue too, while an entry that needs a tuple and a row with no project is never met
// without that row.
func (entry catalogueEntry) holdsWithoutRow() bool {
	return entry.Via == nil
}
