package refs

import (
	"context"
	"encoding/json"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/access"
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
	// predicate someone stated. The zero value of nodeVisibility compiles as a predicate too: its
	// zero scope answers every node to the server's own viewer alone (nodeScope), a rule nobody
	// chose, so the entry carries whether it was chosen rather than defaulted. init refuses the
	// registry over an entry whose stated is false.
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
		// their own loader reads. Each is a project's, or a conversation's, as its row states,
		// except an item's block and a component, whose address names their owner.
		"issue":         resolvedKind(loadIssueNodes, nodeVisibility{scope: projectScope}),
		"artifact":      resolvedKind(loadArtifactNodes, nodeVisibility{fromAddress: artifactStoreAddress, scope: projectScope}),
		"ask":           resolvedKind(loadAskNodes, nodeVisibility{scope: projectScope}),
		"comment":       resolvedKind(loadCommentNodes, nodeVisibility{scope: projectScope}),
		"message":       resolvedKind(loadMessageNodes, nodeVisibility{scope: projectScope}),
		"component":     resolvedKind(loadComponentNodes, nodeVisibility{scope: componentScope}),
		"block":         resolvedKind(loadBlockNodes, nodeVisibility{scope: projectScope}),
		"ask_block":     resolvedKind(loadAskBlockNodes, nodeVisibility{scope: ownerItemScope}),
		"comment_block": resolvedKind(loadCommentBlockNodes, nodeVisibility{scope: ownerItemScope}),
		"message_block": resolvedKind(loadMessageBlockNodes, nodeVisibility{scope: ownerItemScope}),
		// The kinds the node and tuple store holds itself (graphstore.KindPerson and its three
		// siblings, loadStoreNodes). None of them carries a `dispatch://` address, so the
		// address rule does not reach them; the four other rules do. None belongs to a project:
		// each takes the access package's clause of its own kind.
		graphstore.KindPerson:  resolvedKind(loadStoreNodes, nodeVisibility{scope: personScope}),
		graphstore.KindSession: resolvedKind(loadStoreNodes, nodeVisibility{scope: sessionScope}),
		graphstore.KindRole:    resolvedKind(loadStoreNodes, nodeVisibility{scope: roleScope}),
		graphstore.KindPage:    resolvedKind(loadStoreNodes, nodeVisibility{scope: pageScope}),
		// The block schema's own nodes, which a type is (migration 20261109051232). Nothing
		// writes one outside a migration, every one of them is project-visible and no read of
		// the graph resolves one, but they are nodes of the store like any other, so the
		// registry answers for them rather than leaving the one read that lists them
		// (pmdoc/schemanodes, through graphstore.NodesOfKind) with no predicate to bind the
		// day a type is reserved for whoever drafted it. They belong to no project.
		"supertag":  unresolvedKind(nodeVisibility{scope: everyViewerScope}),
		"attribute": unresolvedKind(nodeVisibility{scope: everyViewerScope}),
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
	// scope is the kind's project and party rule (nodeScope): which of its nodes the reader's
	// viewer may see, beside the author rule every kind shares. Every entry states one.
	scope nodeScope
}

// artifactStoreAddress is the ref_key of the artifact whose uuid expr names, as text: an edge's
// from side, which holds an artifact's uuid in a text column beside other kinds' ids.
func artifactStoreAddress(expr string) string {
	return `(select store_artifact.ref_key from artifacts store_artifact where store_artifact.id::text = ` + expr + `)`
}

// artifactRefKeyOf is the ref_key of the artifact whose id uuidExpr, a uuid, names: one probe of
// the artifacts primary key, which artifactStoreAddress's text comparison cannot make.
func artifactRefKeyOf(uuidExpr string) string {
	return `(select ref_key_of.ref_key from artifacts ref_key_of where ref_key_of.id = ` + uuidExpr + `)`
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

// VisibilityArg is the one argument every rendered predicate binds, as jsonb: the author the
// acting credential proved, as graphstore's one author rule compares it (`author`), and what the
// reader's viewer sees, in the shape the access package's clauses read ("person", "session",
// "open", "all", "sees", "private", and "lists", the credential's own list, absent when it lists
// none). A read binds it once and names its placeholder in each predicate it renders.
//
// A reader with no viewer is the closed viewer: it sees no project and no party, is not open, and
// reads as though some project were private, so a read that reached the store without the
// request's viewer answers nothing rather than everything.
func VisibilityArg(who model.Actor) any {
	argument := visibilityArgument{Author: graphstore.ActingCredential(who), Sees: []string{}, Private: true}
	if viewer := who.Viewer; viewer != nil {
		argument.Person, argument.Session = viewer.Person, viewer.Session
		argument.Open, argument.All, argument.Private = viewer.Open, viewer.All, viewer.AnyPrivate
		if viewer.Sees != nil {
			argument.Sees = viewer.Sees
		}
		argument.Lists = viewer.Lists
	}
	encoded, err := json.Marshal(argument)
	if err != nil {
		// Strings, booleans and a raw author graphstore encoded: json.Marshal has nothing here to
		// fail on.
		panic("refs: encode the visibility argument: " + err.Error())
	}
	return encoded
}

// visibilityArgument is VisibilityArg's jsonb. person and session are absent for a viewer that
// names none, so a clause's `->>'person'` reads null, which equals nothing, rather than the empty
// name; lists is absent for a credential that lists none, so every such viewer binds the bytes it
// bound before the member existed.
type visibilityArgument struct {
	Author  json.RawMessage `json:"author"`
	Person  string          `json:"person,omitempty"`
	Session string          `json:"session,omitempty"`
	Sees    []string        `json:"sees"`
	Open    bool            `json:"open"`
	All     bool            `json:"all"`
	Private bool            `json:"private"`
	Lists   []string        `json:"lists,omitempty"`
}

// VisibleKind is the predicate of one node kind: the SQL that is true of a node of that kind
// the reader may see. address is the SQL naming the node's id as `graph_nodes` keys it, a column
// of the read's own query, and param the placeholder the visibility argument is bound at
// (VisibilityArg).
//
// The test is the author rule over the node's row and the kind's scope (nodeVisibility.scope).
// A node of a kind whose row decides its scope, which the store holds no row for, is visible to
// an open viewer while no project is private, as every node Dispatch wrote before this store
// existed was: so a node no write has recorded fails closed the moment any project is private.
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
	return VisibilityMarker + kind + ` */(` +
		visibleWhere(`'`+kind+`'`, address, param, kindRowScope(kind, visibleNode, param), kindWithoutRowScope(kind, param)) +
		` and ` + kindAddressScope(kind, address, param) + `)`
}

// VisibleEnd is the predicate over an end of an edge, whose node kind is a column rather than a
// literal: one read of `graph_nodes` keyed on that column and on the id beside it, with the
// address rewrites the registry declares applied per kind, and each kind's scope chosen by the
// column (scopeByKind). byRefKey says the ids are the ones the to side of an edge carries, which
// are already the ids the store keys a node by; the from side carries an artifact's uuid instead,
// and that kind's rewrite translates it.
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
	rowScope := scopeByKind(visibleNode+`.kind`, "true", func(kind string) string { return kindRowScope(kind, visibleNode, param) })
	withoutRow := scopeByKind(kindExpr, "true", func(kind string) string { return kindWithoutRowScope(kind, param) })
	addressScope := scopeByKind(kindExpr, "true", func(kind string) string { return kindAddressScope(kind, address, param) })
	return VisibilityMarker + `every kind */(` + visibleWhere(kindExpr, address, param, rowScope, withoutRow) + ` and ` + addressScope + `)`
}

// VisibleRow is the predicate over a `graph_nodes` row a read already holds, for the reads that
// walk the store's own table (loadStoreNodes, a person's page): the same test, over the row's
// own kind, with no second read of the table to make it. A row of a kind the registry does not
// hold answers false, as VisibleKindRow refuses one.
func VisibleRow(alias, param string) string {
	scope := scopeByKind(alias+`.kind`, "false", func(kind string) string { return kindHeldScope(kind, alias, param) })
	return VisibilityMarker + `the row's own kind */(` + authorRule(alias, param) + ` and ` + scope + `)`
}

// VisibleKindRow is one node kind's predicate over a `graph_nodes` row a read already holds:
// the test VisibleKind makes, over the row a read's own join or `exists` has already found, so
// a read that must read the node's row anyway makes both tests out of one read of it - the
// count's tuple arms, which answer no edge of a node whose state is gone, are that read
// (backlink_counts.go's liveVisibleNode). A kind the registry does not hold answers false,
// exactly as VisibleKind refuses it, so a read over a kind with no predicate counts nothing
// rather than everything.
//
// The rule VisibleKind states for a node the store holds no row for is the one difference, and
// it is the caller's to keep: a test over a row is a test of a row that is there, and a read
// binds this only where it answers nothing for a node that has none.
func VisibleKindRow(kind, alias, param string) string {
	if _, held := nodeRegistry[kind]; !held {
		return "false"
	}
	return VisibilityMarker + kind + ` */(` + authorRule(alias, param) + ` and ` + kindHeldScope(kind, alias, param) + `)`
}

// visibleNode is the alias visibleWhere reads the node's row under.
const visibleNode = "visible_node"

// authorRule is the author rule over a node's row: a node reserved for its author answers to
// that author alone (graphstore.AuthorIsSQL).
func authorRule(alias, param string) string {
	return `(` + alias + `.visibility <> '` + graphstore.VisibilityAuthor +
		`' or ` + graphstore.AuthorIsSQL(alias+`.made_by`, param) + `)`
}

// visibleWhere is the row half every predicate over an address renders: the store's row for the
// node, if it holds one, read under visibleNode, passes the author rule and rowScope; a node it
// holds no row of answers withoutRow.
//
// It is a scalar subquery and not an `exists`, so the planner keeps it a sub-plan over the
// `graph_nodes` primary key rather than flattening it into an anti-join that reads the whole
// table: every read that binds this is keyed on an index of its own, and a count that gained a
// sequential scan of the store would be paid on every list.
func visibleWhere(kindExpr, address, param, rowScope, withoutRow string) string {
	return `coalesce((select ` + authorRule(visibleNode, param) + ` and ` + rowScope + `
		from graph_nodes ` + visibleNode + ` where ` + visibleNode + `.kind = ` + kindExpr + ` and ` + visibleNode + `.id = ` + address + `), ` +
		withoutRow + `)`
}

// scopeByKind is one CASE over kindExpr that answers, for each kind the registry holds, the rule
// rule renders for it, and otherwise for any kind it does not. Kinds whose rules render alike
// share one branch, so the statement carries each rule once, and a branch equal to otherwise is
// left to it.
func scopeByKind(kindExpr, otherwise string, rule func(kind string) string) string {
	var order []string
	kindsOf := map[string][]string{}
	for _, kind := range VisibilityKinds() {
		rendered := rule(kind)
		if rendered == otherwise {
			continue
		}
		if _, seen := kindsOf[rendered]; !seen {
			order = append(order, rendered)
		}
		kindsOf[rendered] = append(kindsOf[rendered], `'`+kind+`'`)
	}
	if len(order) == 0 {
		return otherwise
	}
	var branches strings.Builder
	branches.WriteString(`(case`)
	for _, rendered := range order {
		branches.WriteString(` when ` + kindExpr + ` in (` + strings.Join(kindsOf[rendered], ", ") + `) then ` + rendered)
	}
	branches.WriteString(` else ` + otherwise + ` end)`)
	return branches.String()
}

// kindRowScope is the half of kind's scope its row decides, over the row alias names, with the
// kind's catalogue arm ORed ahead of it.
func kindRowScope(kind, alias, param string) string {
	rule := nodeRegistry[kind].visibility.scope.overRow(alias, param)
	if entry, catalogued := catalogueEntryOf(kind); catalogued {
		rule = `(` + entry.overRow(alias) + ` or ` + rule + `)`
	}
	return rule
}

// kindWithoutRowScope is what kind's scope answers for a node the store holds no row of.
func kindWithoutRowScope(kind, param string) string {
	if entry, catalogued := catalogueEntryOf(kind); catalogued && entry.holdsWithoutRow() {
		return "true"
	}
	return nodeRegistry[kind].visibility.scope.withoutRow(param)
}

// kindAddressScope is the half of kind's scope the node's address decides.
func kindAddressScope(kind, address, param string) string {
	return nodeRegistry[kind].visibility.scope.overAddress(address, param)
}

// kindHeldScope is kind's whole scope over a row a read holds, whose address is the row's id.
func kindHeldScope(kind, alias, param string) string {
	row, address := kindRowScope(kind, alias, param), kindAddressScope(kind, alias+`.id`, param)
	switch {
	case address == "true":
		return row
	case row == "true":
		return address
	}
	return `(` + row + ` and ` + address + `)`
}

// nodeScope is one node kind's project and party rule, the half of its visibility the reader's
// viewer decides beside the author rule every kind shares. A scope is decided by the node's row
// (row), what project or parties the write that stored the node stated, or by the node's address
// (address), which names the project or the person, session or role the node belongs to. The
// zero scope is one nobody stated, which answers every node to the server's own viewer alone and
// which TestEveryNodeKindAnswersAVisibilityPredicate refuses, naming the kind.
//
// Each is the private-projects spec's v34 rule: a node of a project to whoever sees that project,
// and a node of no project as today for an open viewer, and otherwise only one's own or a party of
// a project one sees. Every clause is the access package's, so the rule has one home.
type nodeScope struct {
	// name says which rule it is, as a failure names it.
	name string
	// row is the rule over the node's row in graph_nodes, its alias row; nil for a scope the
	// address decides alone. A node of such a kind the store holds no row of answers unscoped.
	row func(row, param string) string
	// address is the rule over the node's address, the id graph_nodes keys it by; nil for a
	// scope the row decides alone.
	address func(address, param string) string
}

// overRow is the scope's rule over a row a read holds. Every rule answers true or false, never
// unknown, as each access clause does: the predicates are coalesced and negated by their readers,
// and an unknown there would read as a row the store does not hold.
func (scope nodeScope) overRow(row, param string) string {
	switch {
	case scope.name == "":
		return access.ViewerFlag(param, "all")
	case scope.row == nil:
		return "true"
	}
	return scope.row(row, param)
}

// withoutRow is what the scope answers for a node the store holds no row of.
func (scope nodeScope) withoutRow(param string) string {
	switch {
	case scope.name == "":
		return access.ViewerFlag(param, "all")
	case scope.row == nil:
		return "true"
	}
	return unscoped(param)
}

// overAddress is the scope's rule over a node's address, true or false as overRow's.
func (scope nodeScope) overAddress(address, param string) string {
	if scope.address == nil {
		return "true"
	}
	return scope.address(address, param)
}

// The scopes every kind of the registry takes one of.
var (
	// projectScope is a node of a project kind - an issue, a document, an ask, a message, a
	// comment or a document's block: the project its row names, else the parties of the
	// conversation it is, else, like a node with no row, unscoped.
	projectScope = nodeScope{name: "project", row: projectRule}
	// ownerItemScope is an ask's, a comment's or a message's block, whose address is
	// `<kind>/<item id>#<block id>`: its item's project rule, read off the item's row.
	ownerItemScope = nodeScope{name: "owner item", address: ownerItemRule}
	// componentScope is an architecture component, whose address is `<project>/<id>`, so it
	// needs no row.
	componentScope = nodeScope{name: "component", address: func(address, param string) string {
		return access.ProjectClause(`split_part(`+address+`, '/', 1)`, param)
	}}
	personScope  = nodeScope{name: "person", address: access.PersonClause}
	sessionScope = nodeScope{name: "session", address: access.SessionClause}
	roleScope    = nodeScope{name: "role", address: access.RoleClause}
	// pageScope is a session's page (`session/<id>`), its session's rule, or a role's page
	// (`role/<name>`), its role's.
	pageScope = nodeScope{name: "page", address: pageRule}
	// everyViewerScope is the block schema's own nodes, which belong to no project and to
	// everyone.
	everyViewerScope = nodeScope{name: "every viewer"}
)

// projectRule is a project kind's rule over its row: the project the row names, else the parties
// of the conversation it is (access.PartyClause, which holds each session party to the session
// content rule as well as to its metadata rule: a file's node, a message's and a block of either
// show what the session wrote, which can hold what it read), else unscoped.
func projectRule(row, param string) string {
	return `(case when ` + row + `.project_key is not null then ` + access.ProjectClause(row+`.project_key`, param) +
		` when ` + row + `.parties is not null then ` + access.PartyClause(row+`.parties`, param) +
		` else ` + unscoped(param) + ` end)`
}

// ownerItemRule is an item block's rule over its address: its item's project rule, the item read
// from graph_nodes by the kind and the id the address spells before its `#`. An item the store
// holds no row of is unscoped. The read sits under the CASE that picks a kind's rule, so it ends
// in `offset 0`, as every subquery the clause adds there does.
func ownerItemRule(address, param string) string {
	item := `split_part(` + address + `, '#', 1)`
	return `coalesce((select ` + projectRule("owner_item", param) + `
		from graph_nodes owner_item
		where owner_item.kind = split_part(` + item + `, '/', 1) and owner_item.id = split_part(` + item + `, '/', 2)
		offset 0), ` + unscoped(param) + `)`
}

// pageRule is a page's rule over its address: a session's page is its session's, a role's page is
// its role's, and a page of anything else is the server's alone.
func pageRule(address, param string) string {
	session, role := graphstore.SessionPageID(""), graphstore.RolePageID("")
	return `(case split_part(` + address + `, '/', 1)` +
		` when '` + graphstore.KindSession + `' then ` + access.SessionClause(`substr(`+address+`, `+strconv.Itoa(len(session)+1)+`)`, param) +
		` when '` + graphstore.KindRole + `' then ` + access.RoleClause(`substr(`+address+`, `+strconv.Itoa(len(role)+1)+`)`, param) +
		` else ` + access.ViewerFlag(param, "all") + ` end)`
}

// unscoped is the rule of a node of a project kind that names neither a project nor parties, or
// that the store holds no row of: the server's own viewer, and an open viewer while no project is
// private, which is every reader of a deployment with no private project, as before projects
// could be private. So such a node fails closed the moment any project is private, and the
// completeness census (graphcensus) is what keeps such nodes from existing.
func unscoped(param string) string {
	return `(` + access.ViewerFlag(param, "all") + ` or (` + access.ViewerFlag(param, "open") + ` and not ` + access.ViewerFlag(param, "private") + `))`
}
