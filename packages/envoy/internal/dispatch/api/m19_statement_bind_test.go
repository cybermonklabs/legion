package api

// The binding half of the reading of one statement's parse tree (m19_statement_tree_test.go):
// which of the rows the tree walk found a clause binds, through the equalities and the constants
// it recorded, and which rows an equality on their own owner column holds to a bound row.

import (
	"maps"
	"slices"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// bind marks every column of the statement a clause binds, then every column an equality holds to
// a bound one, until nothing more is bound.
func (a *m19Analysis) bind() {
	a.spreadConstants()
	for _, use := range a.uses {
		span, inside := a.spanAt(use.location)
		if inside && span.binds && use.ref.item.table == "" && !use.ref.item.parameter {
			// A clause over a column of a subselect binds that column's value: a row whose own
			// key equals it is one the clause passed (`c.id = r.id::uuid`, r the arms the
			// registry's predicate read).
			use.ref.item.bind(use.ref.column)
		}
		for _, ref := range a.baseRefs(use.ref) {
			if inside {
				// Every fragment answers false where the row it reads is not there, so a join
				// that answers its row as null answers it to nobody.
				ref.item.rejected = true
			}
			if !inside || !span.binds {
				continue
			}
			if ref.item.parameter && !ref.item.viewer || a.schema.binds[ref.item.table][ref.column] {
				ref.item.bind(ref.column)
			}
		}
	}
	for _, equal := range a.equals {
		if equal.inJoin {
			continue
		}
		for _, side := range []m19Ref{equal.left, equal.right} {
			for _, ref := range a.baseRefs(side) {
				ref.item.rejected = true
			}
		}
	}
	for changed := true; changed; {
		changed = false
		for _, equal := range a.equals {
			filters := equal.filter()
			for index, pair := range [][2]m19Ref{{equal.left, equal.right}, {equal.right, equal.left}} {
				if equal.oneWay && index == 0 {
					continue
				}
				from, to := pair[0], pair[1]
				// The equality filters the rows of the item it names, which may be a subselect
				// or a common table expression: every row of a table that reaches the answer
				// through it is one whose column met the equality.
				if !to.item.parameter && !slices.Contains(filters, to.item) {
					continue
				}
				if args, coalesced := a.coalesceOf(to, map[m19Ref]bool{}); coalesced {
					changed = a.bindOwned(from, args) || changed
					continue
				}
				for _, target := range a.baseRefs(to) {
					if target.item.boundBy[target.column] || !a.carries(from, target) {
						continue
					}
					target.item.bind(target.column)
					changed = true
				}
			}
		}
		for _, held := range a.coalesces {
			args := make([]m19Ref, 0, len(held.args))
			for _, arg := range held.args {
				if base := a.oneBase(arg); base != nil {
					args = append(args, *base)
				}
			}
			if len(args) != len(held.args) {
				continue
			}
			filters := held.filter()
			if row, owned := a.rowOwnedBy(args); owned && slices.Contains(filters, row.item) {
				changed = a.bindOwned(held.other, args) || changed
			}
			// The other way: a coalesce of bound rows' own columns is a value of a bound row, so
			// the row whose own key it is is bound (`p.key = coalesce(i.project_key,
			// ar.project_key)` over the ask's issue and document).
			target := a.oneBase(held.other)
			if target == nil || target.item.parameter || target.item.boundBy[target.column] ||
				!a.schema.plain[target.item.table][target.column] || !slices.Contains(filters, target.item) {
				continue
			}
			if !slices.ContainsFunc(args, func(arg m19Ref) bool { return !a.holds(arg) }) {
				target.item.bind(target.column)
				changed = true
			}
		}
		for _, derived := range a.derivations {
			target := derived.target
			if target.item.boundBy[target.column] || !slices.Contains(derived.filter(), target.item) || !a.valueHolds(derived.value) {
				continue
			}
			target.item.bind(target.column)
			changed = true
		}
		for _, arms := range a.disjunctions {
			changed = a.bindDisjunction(arms) || changed
		}
	}
}

// bindDisjunction binds a row every arm of an OR holds by an equality of its own, on the column
// the first arm binds it by, and answers whether it bound something new.
func (a *m19Analysis) bindDisjunction(arms [][]*m19Equality) bool {
	if len(arms) == 0 {
		return false
	}
	for _, equal := range arms[0] {
		for _, candidate := range a.boundTargets(equal) {
			if candidate.item.boundBy[candidate.column] {
				continue
			}
			held := !slices.ContainsFunc(arms[1:], func(arm []*m19Equality) bool {
				return !slices.ContainsFunc(arm, func(other *m19Equality) bool {
					return slices.ContainsFunc(a.boundTargets(other), func(target m19Ref) bool { return target.item == candidate.item })
				})
			})
			if held {
				candidate.item.bind(candidate.column)
				return true
			}
		}
	}
	return false
}

// boundTargets are the columns of tables one equality binds on its own: those of the side it
// filters that the other side carries a binding to.
func (a *m19Analysis) boundTargets(equal *m19Equality) []m19Ref {
	var found []m19Ref
	filters := equal.filter()
	for index, pair := range [][2]m19Ref{{equal.left, equal.right}, {equal.right, equal.left}} {
		if equal.oneWay && index == 0 {
			continue
		}
		from, to := pair[0], pair[1]
		if to.item.parameter || !slices.Contains(filters, to.item) {
			continue
		}
		for _, target := range a.baseRefs(to) {
			if a.carries(from, target) {
				found = append(found, target)
			}
		}
	}
	return found
}

// valueHolds answers whether a column or parameter holds a value a clause bound: a parameter or a
// subselect's column a clause read, or a column every row it comes from is bound for.
func (a *m19Analysis) valueHolds(ref m19Ref) bool {
	if ref.item == nil {
		return false
	}
	if a.boundValue(ref, map[m19Ref]bool{}) {
		return true
	}
	bases := a.baseRefs(ref)
	return len(bases) > 0 && !slices.ContainsFunc(bases, func(base m19Ref) bool { return !a.holds(base) })
}

// boundValue answers whether a parameter, or a column of a subselect, holds a value a clause
// bound: the column itself, or the one column it comes from.
func (a *m19Analysis) boundValue(ref m19Ref, seen map[m19Ref]bool) bool {
	if ref.item == nil || seen[ref] {
		return false
	}
	seen[ref] = true
	switch {
	case ref.item.parameter:
		return !ref.item.viewer && ref.item.boundBy[ref.column]
	case ref.item.table != "":
		return false
	case ref.item.boundBy[ref.column]:
		return true
	}
	for _, output := range ref.item.columns() {
		if output.name == ref.column && len(output.sources) == 1 {
			return a.boundValue(output.sources[0], seen)
		}
	}
	return false
}

// coalesceOf is the columns of tables a column of a subselect or a common table expression
// coalesces (`coalesce(i.project_key, ar.project_key) as project_key`), each one column of one
// table, and whether the column is one.
func (a *m19Analysis) coalesceOf(ref m19Ref, seen map[m19Ref]bool) ([]m19Ref, bool) {
	if ref.item == nil || ref.item.table != "" || ref.item.parameter || seen[ref] {
		return nil, false
	}
	seen[ref] = true
	for _, output := range ref.item.columns() {
		if output.name != ref.column {
			continue
		}
		if len(output.coalesce) > 0 {
			args := make([]m19Ref, 0, len(output.coalesce))
			for _, arg := range output.coalesce {
				base := a.oneBase(arg)
				if base == nil {
					return nil, false
				}
				args = append(args, *base)
			}
			return args, true
		}
		if len(output.sources) == 1 {
			return a.coalesceOf(output.sources[0], seen)
		}
	}
	return nil, false
}

// bindOwned binds the row whose owners a coalesce reads, where the value it equals holds:
// `coalesce(i.project_key, ar.project_key) = p.key`, p bound, over asks a joined to i and ar by
// its own issue and document, is the rule ItemOwnerClause states, so the ask is bound. It answers
// whether it bound something new.
func (a *m19Analysis) bindOwned(value m19Ref, args []m19Ref) bool {
	if !a.valueHolds(value) {
		return false
	}
	row, owned := a.rowOwnedBy(args)
	if !owned || row.item.boundBy[row.column] {
		return false
	}
	row.item.bind(row.column)
	return true
}

// rowOwnedBy is the one row every column of a coalesce is the project of: the row itself, or the
// owner the row names by an owner column of its own, joined by that column. It answers the row
// and the owner column, and whether there is one.
func (a *m19Analysis) rowOwnedBy(args []m19Ref) (m19Ref, bool) {
	var found m19Ref
	for index, arg := range args {
		if !a.schema.plain[arg.item.table][arg.column] {
			return m19Ref{}, false
		}
		rows := a.rowsOwnedThrough(arg)
		if index == 0 {
			if len(rows) != 1 {
				return m19Ref{}, false
			}
			found = rows[0]
			continue
		}
		if !slices.ContainsFunc(rows, func(row m19Ref) bool { return row.item == found.item }) {
			return m19Ref{}, false
		}
	}
	return found, found.item != nil
}

// rowsOwnedThrough are the rows of the statement whose owner arg's row is, each with the owner
// column that joins it: arg's row itself (by arg's column), every row an equality holds to arg's
// row by an owner column of its own, and every row a coalesce of its own owner columns names
// arg's row by (`a.id = coalesce(k.artifact_id, k.block_artifact_id)`).
func (a *m19Analysis) rowsOwnedThrough(arg m19Ref) []m19Ref {
	var rows []m19Ref
	for _, equal := range a.equals {
		for _, pair := range [][2]m19Ref{{equal.left, equal.right}, {equal.right, equal.left}} {
			child, parent := a.oneBase(pair[0]), a.oneBase(pair[1])
			if child == nil || parent == nil || parent.item != arg.item || child.item == arg.item {
				continue
			}
			if owner := a.schema.owners[child.item.table][child.column]; owner == (m19Owner{table: parent.item.table, column: parent.column}) {
				rows = append(rows, *child)
			}
		}
	}
	for _, held := range a.coalesces {
		parent := a.oneBase(held.other)
		if parent == nil || parent.item != arg.item || len(held.args) == 0 {
			continue
		}
		owner := m19Owner{table: parent.item.table, column: parent.column}
		child := a.oneBase(held.args[0])
		if child == nil || child.item == arg.item || slices.ContainsFunc(held.args, func(column m19Ref) bool {
			base := a.oneBase(column)
			return base == nil || base.item != child.item || a.schema.owners[base.item.table][base.column] != owner
		}) {
			continue
		}
		rows = append(rows, *child)
	}
	if len(rows) == 0 {
		rows = append(rows, arg)
	}
	return rows
}

// carries answers whether a column of the statement carries a binding to target across an
// equality: every column of a table its value comes from is bound and matches target. A column
// of a recursive common table expression comes from target itself too, through the expression's
// own recursion; a row the recursion adds is bound when the row it was reached from is, so that
// source is the induction's and not one the binding needs.
func (a *m19Analysis) carries(from, target m19Ref) bool {
	if a.boundValue(from, map[m19Ref]bool{}) {
		return a.schema.binds[target.item.table][target.column]
	}
	sources := slices.DeleteFunc(a.baseRefs(from), func(source m19Ref) bool { return source.item == target.item })
	if len(sources) == 0 {
		return false
	}
	for _, source := range sources {
		if !a.holds(source) || !a.matches(source, target) {
			return false
		}
	}
	return true
}

// matches answers whether an equality between two columns holds the rows of one to the rows of
// the other (m19Schema.matches). A parameter a clause may bind matches every column that binds
// a row or a node: a statement that binds the issue its parameter names holds every row whose
// own issue key is that parameter.
func (a *m19Analysis) matches(left, right m19Ref) bool {
	for _, pair := range [][2]m19Ref{{left, right}, {right, left}} {
		if pair[0].item.parameter {
			return !pair[0].item.viewer && !pair[1].item.parameter && a.schema.binds[pair[1].item.table][pair[1].column]
		}
	}
	return a.schema.matches(left, right)
}

// holds answers whether a bound column carries its binding across an equality: a parameter or a
// subselect's column a clause binds, one half of a node's address that is bound, or any column of
// a row that is bound by what owns it or which row it is. A row bound by its owner vouches for
// its own address (the block that states a link), never for the address it points at; a
// membership tuple bound by what it is a member of vouches for its member.
func (a *m19Analysis) holds(ref m19Ref) bool {
	if ref.item.parameter || ref.item.table == "" {
		return a.boundValue(ref, map[m19Ref]bool{})
	}
	if a.schema.inAddress(ref.item.table, ref.column) && !a.schema.plain[ref.item.table][ref.column] {
		if ref.item.boundBy[ref.column] {
			return true
		}
		if a.membership(ref.item) {
			return a.isBound(ref.item)
		}
		return m19OwnAddresses[a.schema.addressOf(ref.item.table, ref.column)] && a.boundByOwner(ref.item)
	}
	return a.isBound(ref.item)
}

// boundByOwner answers whether a clause or an equality bound an item by a column that names what
// owns it or which row it is.
func (a *m19Analysis) boundByOwner(item *m19Item) bool {
	for column := range item.boundBy {
		if a.schema.plain[item.table][column] {
			return true
		}
	}
	return false
}

// m19OwnAddresses are the prefixes of the addresses that name the node a row is of, rather than a
// node the row points at: a tuple's source, a store's own node, a block's own node.
var m19OwnAddresses = map[string]bool{"": true, "from": true, "node": true, "from_node": true}

// m19MembershipRelations are the tuple kinds that make their source a member of their target, so
// the target owns the source: a plan's or a step's part of its issue (workflowstore), a block's
// part of its document and of the block holding it.
var m19MembershipRelations = map[string]string{
	graphstore.TuplePartOf: "the source is part of the target (graphstore, workflowstore.RelationPartOf)",
	refs.TupleInDocument:   "the block is in the document",
}

// membership answers whether an item is a tuple the statement holds to membership relations
// alone, whose target owns its source.
func (a *m19Analysis) membership(item *m19Item) bool {
	addresses := a.schema.addresses[item.table]
	if _, to := addresses["to"]; !to {
		return false
	}
	values := a.constants[m19Ref{item: item, column: "kind"}]
	return len(values) > 0 && !slices.ContainsFunc(values, func(value string) bool {
		_, member := m19MembershipRelations[value]
		return !member
	})
}

// isBound answers whether an item's rows are held to the projects the reader sees: a column that
// names what owns them or which row each is; for a membership tuple, its target; or, for a row
// that names nodes of the graph, each side of it that can name a node a project owns. The
// addresses that name the row's own node (m19OwnAddresses: a block link's node_address and
// from_node) are one side, bound where one of them is; every address the row points at is a side
// of its own.
func (a *m19Analysis) isBound(item *m19Item) bool {
	if a.boundByOwner(item) {
		return true
	}
	if a.membership(item) && slices.ContainsFunc(a.schema.addresses[item.table]["to"].ids, func(id string) bool { return item.boundBy[id] }) {
		return true
	}
	bound := func(address m19Address) bool {
		return slices.ContainsFunc(address.ids, func(id string) bool { return item.boundBy[id] })
	}
	live, ownLive, ownBound := 0, false, false
	for prefix, address := range a.schema.addresses[item.table] {
		if !a.live(item, address) {
			continue
		}
		live++
		if m19OwnAddresses[prefix] {
			ownLive, ownBound = true, ownBound || bound(address)
			continue
		}
		if !bound(address) {
			return false
		}
	}
	return live > 0 && (!ownLive || ownBound)
}

// excused answers whether an item reads no project's row at all: a row that names nodes of the
// graph, each address of which the statement holds to kinds no project owns (m19KindsOfNoProject)
// - a priority, a person, a value - and that names no object by an owner column of its own; or a
// row of a store of nodes (graph_nodes) whose one address, its own, is held so, since its project
// is that node's.
func (a *m19Analysis) excused(item *m19Item) bool {
	addresses := a.schema.addresses[item.table]
	if len(addresses) == 0 {
		return false
	}
	for _, address := range addresses {
		if a.live(item, address) {
			return false
		}
	}
	if _, own := addresses[""]; own && len(addresses) == 1 {
		return true
	}
	for _, owner := range a.schema.owners[item.table] {
		if owner.table != "" {
			return false
		}
	}
	return true
}

// live answers whether an address of an item can name a node a project owns: its kind column is
// held to no constant, or to one kind that is not one of m19KindsOfNoProject.
func (a *m19Analysis) live(item *m19Item, address m19Address) bool {
	values := a.constants[m19Ref{item: item, column: address.kind}]
	return len(values) == 0 || slices.ContainsFunc(values, func(value string) bool {
		_, none := m19KindsOfNoProject[value]
		return !none
	})
}

// spreadConstants carries the constants a conjunct holds a column to across every equality of
// the statement, so `n.kind = t.from_kind and t.from_kind in ('step', 'plan')` holds both.
func (a *m19Analysis) spreadConstants() {
	for ref, values := range maps.Clone(a.constants) {
		for _, base := range a.baseRefs(ref) {
			if base != ref {
				a.pin(base, values...)
			}
		}
	}
	for changed := true; changed; {
		changed = false
		for _, equal := range a.equals {
			left, right := a.oneBase(equal.left), a.oneBase(equal.right)
			if left == nil || right == nil {
				continue
			}
			for _, pair := range [][2]*m19Ref{{left, right}, {right, left}} {
				for _, value := range a.constants[*pair[0]] {
					if !slices.Contains(a.constants[*pair[1]], value) {
						a.pin(*pair[1], value)
						changed = true
					}
				}
			}
		}
	}
}

// baseRefs are the columns of tables one column of the statement answers: itself, for a column of
// a table or a parameter, and every column its value comes from, for a column of a subselect or a
// common table expression.
func (a *m19Analysis) baseRefs(ref m19Ref) []m19Ref {
	return a.baseRefsSeen(ref, map[m19Ref]bool{})
}

func (a *m19Analysis) baseRefsSeen(ref m19Ref, seen map[m19Ref]bool) []m19Ref {
	if ref.item == nil || seen[ref] {
		return nil
	}
	seen[ref] = true
	if ref.item.table != "" || ref.item.parameter {
		return []m19Ref{ref}
	}
	var found []m19Ref
	for _, output := range ref.item.columns() {
		if output.name != ref.column {
			continue
		}
		for _, source := range output.sources {
			found = append(found, a.baseRefsSeen(source, seen)...)
		}
	}
	return found
}

// oneBase is the one column of a table a column of the statement answers, nil where it answers
// none or several: a column of a set operation answers a column of either arm, and holding one
// of them holds no row of the other.
func (a *m19Analysis) oneBase(ref m19Ref) *m19Ref {
	found := a.baseRefs(ref)
	if len(found) != 1 {
		return nil
	}
	return &found[0]
}
