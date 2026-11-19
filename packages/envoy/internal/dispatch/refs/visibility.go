package refs

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// Every read of the graph answers only the nodes the acting person may see. A node carries that
// in `graph_nodes.visibility`: `project`, which everyone who may read the project reads and which
// every node of most kinds holds, or `author`, which reserves the node for whoever wrote it and
// which the store takes only for a kind the registry flags (nodeKind.authorOnly). A node
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
//   - The constructors below state the predicate: resolvedKind and unresolvedKind take it as an
//     argument, so a kind registered with a loader and no predicate is a call with an argument
//     missing and the package does not build, and authoredKind and credentialAuthoredKind each
//     make the one predicate their name says. This is the line a kind added the way every kind
//     here was added meets.
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
	// authorOnly says every read of the kind binds its predicate, so the kind may hold a node
	// reserved for its author: init hands every kind flagged so to the store, which refuses such
	// a node of any other kind (graphstore.AdmitAuthorOnly). It is set by authoredKind and
	// credentialAuthoredKind alone, the constructors of the kinds dispatch://INFRA-1950 adds that
	// take `author` - interaction, lens, lens_block, risk and constraint - each called in the
	// commit that registers its kind; a lens_kind is always project-visible and takes neither.
	authorOnly bool
	// stated is true of an entry one of the constructors made, which is every entry whose
	// predicate someone stated: the zero value of nodeVisibility is a predicate in its own
	// right, the rule most kinds take, so the entry carries whether it was chosen rather than
	// defaulted. init refuses the registry over an entry whose stated is false.
	stated bool
}

// resolvedKind registers a node kind the graph addresses: the loader that reads its nodes, and
// the predicate every read of the kind binds. The kind takes no node reserved for its author.
func resolvedKind(load nodeLoader, visibility nodeVisibility) nodeKind {
	return nodeKind{load: load, visibility: visibility, stated: true}
}

// unresolvedKind registers a node kind the store holds rows of that no read of the graph
// resolves into a node. It answers a predicate like every other kind and carries no loader.
func unresolvedKind(visibility nodeVisibility) nodeKind {
	return nodeKind{visibility: visibility, stated: true}
}

// authoredKind registers a node kind that may hold a node reserved for its author and whose
// predicate compares the author a credential proved (comparesAuthor), the one rule every kind
// dispatch://INFRA-1149 registers compares too: the loader that reads its nodes, every one of
// which binds that predicate. A lens, a block of a lens's finding (lens_block), a risk and a
// constraint are registered by it (dispatch://INFRA-1950's R19), so a person's cookie, her
// personal token and a session the broker verified as hers read what any of them wrote.
func authoredKind(load nodeLoader) nodeKind {
	return nodeKind{load: load, visibility: nodeVisibility{compares: comparesAuthor}, authorOnly: true, stated: true}
}

// credentialAuthoredKind registers a node kind that may hold a node reserved for its author and
// whose predicate compares the whole acting credential (comparesCredential): the loader that
// reads its nodes, every one of which binds that predicate. An interaction is registered by it,
// since a run is read by the credential that made it, as the going stream reads its run
// (model.Actor.SameCredential), and a run whose author is nobody by nobody.
func credentialAuthoredKind(load nodeLoader) nodeKind {
	return nodeKind{load: load, visibility: nodeVisibility{compares: comparesCredential}, authorOnly: true, stated: true}
}

// nodeRegistry is every node kind this build knows: how each one resolves, and the visibility
// predicate every read of it binds. These are node kinds, the ends of an edge, not edge kinds:
// a node kind a newer build adds to the graph is addressable, listed and counted only once it
// has an entry here. The keys are the kinds graph_edges stores; an ask's, a comment's or a
// message's block is stored as ask_block, comment_block or message_block and resolves to a
// node of kind block, as a document's block does. A finding of a lens is stored there as
// lens_block and resolves to a node of that kind, since no document and no item holds it; the
// store keys it as a block (nodeVisibility.storedKind).
//
// It is filled in init rather than in its own declaration because the loaders it holds render,
// through VisibleKind, the predicates this same registry answers: as the initializer of the
// variable, that reference is an initialization cycle the compiler refuses, while at init it
// is what it is at a read - a loader that reads the registry long after it is built.
var nodeRegistry map[string]nodeKind

// nodeKinds is every node kind nodeRegistry resolves, sorted, as the reads that filter
// graph_edges in SQL bind it. BacklinkCounts binds it on its mention arm alone, the one arm
// whose from_kind is a column; every other arm it reads fixes from_kind as a literal, or a list
// of literals, this list holds, so the filter would drop nothing there. A kind the registry
// carries no loader for is the address of no node a read of the graph answers, so it is none of
// these.
var nodeKinds []string

// credentialComparedKinds is every node kind of nodeRegistry whose predicate compares the whole
// credential (comparesCredential), sorted: what a predicate over a node whose kind is a column
// reads to pick the comparison of each node's own kind (authorIsOfRow).
var credentialComparedKinds []string

// registrySQL is the SQL the predicates over a node whose kind is a column splice from the
// registry's own lists, rendered once the registry is built (deriveRegistryLists) rather than on
// every call of those predicates, which every read of the graph makes.
var registrySQL struct {
	// kinds is every kind the registry holds a predicate for, as the literals of an `in` list
	// (VisibleNode).
	kinds string
	// credentialCompared is credentialComparedKinds as the literals of an `in` list
	// (authorIsOfRow).
	credentialCompared string
	// storedRewrites is the `when` arm of storedKindSQL's case for each entry the store keys
	// under another kind, empty where no entry is.
	storedRewrites string
}

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
		// The kinds the node and tuple store holds itself (graphstore.KindPerson and its three
		// siblings, loadStoreNodes). None of them carries a `dispatch://` address, so the
		// address rule does not reach them; the four other rules do.
		graphstore.KindPerson:  resolvedKind(loadStoreNodes, nodeVisibility{}),
		graphstore.KindSession: resolvedKind(loadStoreNodes, nodeVisibility{}),
		graphstore.KindRole:    resolvedKind(loadStoreNodes, nodeVisibility{}),
		graphstore.KindPage:    resolvedKind(loadStoreNodes, nodeVisibility{}),
		// The kinds of dispatch://INFRA-1950 the store holds alone, each registered in the commit
		// that first writes a node of it (migration 20261116123354 writes the first two, the
		// reasoning write of api the next three, and the going POST of api the last). A lens kind
		// is a node of the catalogue, always project-visible, so its entry takes no node reserved
		// for its author; a constraint, a lens, a finding of a lens and a risk may each be reserved
		// for its author, and each one's predicate compares the author a credential proved, as
		// every kind dispatch://INFRA-1149 registers does. A finding is stored as a node of kind
		// block at its own id, `lens/<uuid>#<block id>`, so its entry reads the store under that
		// kind (storedAs). A lens, a finding and a risk are read by one loader, which takes the
		// kind it reads (loadReasoningNodes). An interaction, one run of the command bar, is
		// always reserved for its author, and its predicate compares the whole credential, as the
		// going stream compares its reader with the run's author.
		graphstore.KindLensKind:    resolvedKind(loadLensKindNodes, nodeVisibility{}),
		graphstore.KindConstraint:  authoredKind(loadConstraintNodes),
		graphstore.KindLens:        authoredKind(loadReasoningNodes),
		graphstore.KindLensBlock:   authoredKind(loadReasoningNodes).storedAs(BlockNodeKind),
		graphstore.KindRisk:        authoredKind(loadReasoningNodes),
		graphstore.KindInteraction: credentialAuthoredKind(loadInteractionNodes),
		// A recorded result of a check (graphstore.KindResult), which recordCheckResult writes
		// beside its check_results row: project-visible, and resolved by no read of the graph yet,
		// so its entry carries no loader.
		graphstore.KindResult: unresolvedKind(nodeVisibility{}),
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
			// rule nobody wrote: the entry was written as a literal instead of through one of
			// the constructors, and took the zero predicate by default.
			panic("refs: the node kind " + kind + " is registered with no visibility predicate: " +
				"every entry of nodeRegistry is made by one of its constructors, each of which states one")
		}
	}
	deriveRegistryLists()
	// The store takes a node reserved for its author of the kinds flagged here and of no other,
	// so this is the one place that decides which kinds those are (graphstore.AdmitAuthorOnly).
	graphstore.AdmitAuthorOnly(authorOnlyKinds()...)
}

// deriveRegistryLists sets what is read off nodeRegistry once it is built: the kinds it resolves
// (nodeKinds), the kinds whose predicate compares the whole credential (credentialComparedKinds),
// and the SQL rendered from them (registrySQL).
func deriveRegistryLists() {
	nodeKinds = registryKinds(func(entry nodeKind) bool { return entry.load != nil })
	credentialComparedKinds = registryKinds(func(entry nodeKind) bool { return entry.visibility.compares == comparesCredential })
	var rewrites strings.Builder
	for _, kind := range VisibilityKinds() {
		if stored := nodeRegistry[kind].visibility.storedKind; stored != "" {
			rewrites.WriteString(` when '` + kind + `' then '` + stored + `'`)
		}
	}
	registrySQL.kinds = quotedKinds(VisibilityKinds())
	registrySQL.credentialCompared = quotedKinds(credentialComparedKinds)
	registrySQL.storedRewrites = rewrites.String()
}

// authorOnlyKinds is every kind of nodeRegistry flagged as one that may hold a node reserved for
// its author (nodeKind.authorOnly), sorted.
func authorOnlyKinds() []string {
	return registryKinds(func(entry nodeKind) bool { return entry.authorOnly })
}

// registryKinds is the registry's keys whose entry keep holds of, sorted.
func registryKinds(keep func(nodeKind) bool) []string {
	var kinds []string
	for _, kind := range VisibilityKinds() {
		if keep(nodeRegistry[kind]) {
			kinds = append(kinds, kind)
		}
	}
	return kinds
}

// comparison is what a kind's predicate compares the author of a node reserved for that author
// with (nodeVisibility.compares).
type comparison uint8

const (
	// comparesAuthor compares the author the acting credential proved with the author the node's
	// `made_by` records, the one rule of dispatch://INFRA-1149 (graphstore.AuthorIsSQL): the
	// person a cookie, a personal token or a session the broker verified proves, else the
	// credential's own id, which counts for a session only where the broker proved that session
	// (model.Actor's Proven), else its workload. It is every kind dispatch://INFRA-1149 registers,
	// and a lens, a block of a lens's finding, a risk and a constraint (authoredKind).
	comparesAuthor comparison = iota
	// comparesCredential compares the whole acting credential with the one that wrote the node
	// (graphstore.AuthorCredentialIsSQL): an interaction.
	comparesCredential
)

// authorIs is the comparison c states of the author a node's `made_by` column records and the
// acting credential bound at param.
func (c comparison) authorIs(column, param string) string {
	if c == comparesCredential {
		return graphstore.AuthorCredentialIsSQL(column, param)
	}
	return graphstore.AuthorIsSQL(column, param)
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
	// storedKind is the kind `graph_nodes` keys the kind's nodes by, and is empty for every kind
	// the store keys under its own name. Only a finding of a lens differs: graph_edges and every
	// reference name it lens_block, at its own id `lens/<uuid>#<block id>`, and the store holds
	// it as a node of kind block at that same id (graphstore.KindLensBlock), so a predicate keyed
	// on lens_block would find no row and answer the finding to everyone (storedAs).
	storedKind string
	// compares is what the predicate compares the author of a node reserved for that author
	// with: the author the acting credential proved, the zero value, which resolvedKind,
	// unresolvedKind and authoredKind state; or the whole acting credential, which
	// credentialAuthoredKind states.
	compares comparison
}

// storedAs is the entry k with its nodes keyed in `graph_nodes` under kind rather than under the
// registry's own key (nodeVisibility.storedKind): a finding of a lens, registered as lens_block
// and stored as a block. Every predicate the registry renders for the entry reads the store's
// row under that kind, at the id the kind is addressed by.
func (k nodeKind) storedAs(kind string) nodeKind {
	k.visibility.storedKind = kind
	return k
}

// StoredKindOf is the kind `graph_nodes` keys the nodes of the registry kind kind by: the entry's
// storedKind where it states one, and kind itself otherwise, a kind the registry does not hold
// among them. A finding of a lens, which every reference names lens_block, is the store's block.
func StoredKindOf(kind string) string {
	if stored := nodeRegistry[kind].visibility.storedKind; stored != "" {
		return stored
	}
	return kind
}

// storedKindSQL is StoredKindOf over a kind that is the SQL kindExpr rather than a literal: the
// kind expression itself where no entry is stored under another kind, and a case over the
// entries that are otherwise (registrySQL.storedRewrites). A node already named by its stored
// kind - a finding as the store's block - passes through it unchanged.
func storedKindSQL(kindExpr string) string {
	if registrySQL.storedRewrites == "" {
		return kindExpr
	}
	return `(case ` + kindExpr + registrySQL.storedRewrites + ` else ` + kindExpr + ` end)`
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

// ResolvesNodeKind is whether a read of the graph answers nodes of kind at all: the registry
// holds a loader for it (nodeKinds). A kind it does not resolve is the address of no node.
func ResolvesNodeKind(kind string) bool {
	return nodeRegistry[kind].load != nil
}

// VisibilityArg is the one argument every rendered predicate binds: the acting credential, as
// the kinds that compare the whole credential read it, with the author it proved beside it, which
// graphstore's one author rule reads (graphstore.ActingCredential). A read binds it once and names
// its placeholder in each predicate it renders, whatever kinds those are.
func VisibilityArg(who model.Actor) any {
	return graphstore.ActingCredential(who)
}

// VisibleKind is the predicate of one node kind: the SQL that is true of a node of that kind
// the acting person may see. address is the SQL naming the node's id as `graph_nodes` keys it,
// a column of the read's own query, and param the placeholder the acting credential is bound at
// (VisibilityArg). A node reserved for its author is compared with the acting credential as the
// kind's registry entry states (nodeVisibility.compares). The row is read under the kind the
// store keys the kind's nodes by (StoredKindOf): a lens_block's under block.
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
	entry, held := nodeRegistry[kind]
	if !held {
		return "false"
	}
	return VisibilityMarker + kind + ` */` +
		visibleWhere(`'`+StoredKindOf(kind)+`'`, address, entry.visibility.compares.authorIs(`visible_node.made_by`, param))
}

// StoredNodeMarker opens the one read of `visibility` that is no test of it: the column a loader
// selects to answer a node's own stored state and visibility (storedNode). It decides nothing
// about which nodes a read answers, which the kind's predicate has already decided, so the census
// of the registry (TestEveryNodeKindAnswersAVisibilityPredicate) tells it from a predicate by
// this marker, and still holds every other `visibility` a statement reads to a predicate the
// registry rendered.
const StoredNodeMarker = "/* the node's stored state and visibility */"

// storedNode is the column every loader of kind selects for the node whose id address names, as
// `graph_nodes` keys it - the address that kind's predicate reads (VisibleKind): the state and
// the visibility the store holds for the node, as a text array of two, or null where the store
// holds no row of it (setStored). It reads `visibility` once, which StoredNodeMarker accounts
// for.
//
// It is a column and not a test, so it runs only for a row the read's own filters kept: one more
// probe of the `graph_nodes` primary key for each node a read answers, beside the predicate's own
// probe of the same row. That is a choice for a loader that answers a handful of nodes. The block
// loader answers thousands - every block of an 8,001-block document, the 5,000 blocks a list of
// links points from - where the second probe cost a read of 5,001 blocks a fifth of its time
// (dispatch://INFRA-1950's c-read-cost), so it reads both from one probe (visibleStoredKind).
func storedNode(kind, address string) string {
	return StoredNodeOf(`'`+StoredKindOf(kind)+`'`, address)
}

// visibleStoredKind is VisibleKind and storedNode of the node of kind whose id address names,
// read in one probe of the node's `graph_nodes` row rather than two, for a loader that answers
// many nodes at once (loadBlockNodes). It answers the lateral join over that row the loader adds
// to its from list; the column that answers storedNode's array from it (StoredRowOf), null where
// the store holds no row; and the predicate over it, VisibleKind's test of the same row under the
// same key with the same comparison of its author (VisibleKindRow), and true where the store
// holds no row. So the loader answers the same nodes with the same state and visibility as the
// pair it replaces, and the join reads `visibility` once under each of the two markers, as that
// pair does.
//
// The join ends in `offset 0`, which keeps it a probe of the primary key per row, as the scalar
// sub-plan it replaces is (visibleWhere): a lateral subquery Postgres may pull up is a join it may
// hash from a scan of the whole store. A kind the registry does not hold joins nothing and
// answers false, as VisibleKind does.
func visibleStoredKind(kind, address, param string) (join, column, predicate string) {
	if _, held := nodeRegistry[kind]; !held {
		return "", "null::text[]", "false"
	}
	const alias, row = "stored_visible", "stored_visible_node"
	join = `left join lateral (select ` + StoredRowOf(row) + ` as held, ` + VisibleKindRow(kind, row, param) + ` as seen
		from graph_nodes ` + row + ` where ` + row + `.kind = '` + StoredKindOf(kind) + `' and ` + row + `.id = ` + address + `
		offset 0) ` + alias + ` on true`
	return join, alias + `.held`, `coalesce(` + alias + `.seen, true)`
}

// StoredNodeOf is storedNode for a node whose kind is the SQL kindExpr rather than a literal, for
// a read outside this package that answers each node's stored state and visibility beside rows
// of several kinds (api's person page): the same column, opened by the same marker. Its answer
// is read by StoredStateOf.
func StoredNodeOf(kindExpr, address string) string {
	return StoredNodeMarker + `(select array[stored_node.state, stored_node.visibility] from graph_nodes stored_node
		where stored_node.kind = ` + kindExpr + ` and stored_node.id = ` + address + `)`
}

// StoredRowOf is the column StoredNodeOf answers, read off the `graph_nodes` row a statement
// already holds as alias rather than by a probe of its own: the row's state and visibility,
// opened by the same marker. A read that joins the node's row anyway selects this (api's lens
// read, visibleStoredKind), so it reads that row once.
func StoredRowOf(alias string) string {
	return StoredNodeMarker + `array[` + alias + `.state, ` + alias + `.visibility]`
}

// StoredStateAndVisibility is the state and the visibility a storedNode column read for a node,
// and live and project where the store holds no row of it, as VisibleKind reads such a node:
// `graph_nodes` is the store's own list, and a node no write has recorded there is as live and as
// project-visible as every node Dispatch wrote before the store existed. A row is two values; ok
// is false for anything else.
func StoredStateAndVisibility(stored []string) (state, visibility string, ok bool) {
	switch len(stored) {
	case 0:
		return graphstore.StateLive, graphstore.VisibilityProject, true
	case 2:
		return stored[0], stored[1], true
	}
	return "", "", false
}

// StoredStateOf is StoredStateAndVisibility of the column a read selected for the node node
// names, failing that read, in the words every read of the column fails in, for a column that
// is not one.
func StoredStateOf(node string, stored []string) (state, visibility string, err error) {
	state, visibility, ok := StoredStateAndVisibility(stored)
	if !ok {
		return "", "", fmt.Errorf("read the stored state and visibility of %s: %d values, want 2", node, len(stored))
	}
	return state, visibility, nil
}

// setStored gives node the state and visibility storedNode read for it (StoredStateOf).
func setStored(node *model.GraphNode, stored []string) (err error) {
	node.State, node.Visibility, err = StoredStateOf(node.Kind+" "+node.ID, stored)
	return err
}

// VisibleEnd is the predicate over an end of an edge, whose node kind is a column rather than a
// literal: one read of `graph_nodes` keyed on that column and on the id beside it, with the
// address rewrites the registry declares applied per kind, the kind read as the store keys it
// (storedKindSQL: an edge's lens_block is the store's block), and the comparison of the kind of
// the row it reads (authorIsOfRow). byRefKey says the ids are the ones the to side of an edge
// carries, which are already the ids the store keys a node by; the from side carries an
// artifact's uuid instead, and that kind's rewrite translates it.
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
	return VisibilityMarker + `every kind */` + visibleWhere(storedKindSQL(kindExpr), address, authorIsOfRow(`visible_node`, param))
}

// VisibleNode is the predicate of a node whose kind is a column of the read's own query rather
// than a literal - the far end of a tuple, say: kindExpr and idExpr are its kind and its id, as
// `graph_nodes` holds them or as the graph addresses them, which differ only for a finding of a
// lens (storedKindSQL). It is the predicate of the node's own kind, with that kind's comparison
// (authorIsOfRow), and false for a node whose kind the registry does not hold, as VisibleKind
// refuses one, so a node of a kind a newer build adds counts for nothing here until its entry
// does. A block's kind is read off its id (graphstore.AddressKind): a finding of a lens is a
// lens_block, a block of a document a block.
//
// A node the store holds no row for is visible, as VisibleKind reads one.
func VisibleNode(kindExpr, idExpr, param string) string {
	return VisibilityMarker + `the row's own kind */(` + graphstore.AddressKindSQL(kindExpr, idExpr) + ` in (` +
		registrySQL.kinds + `) and ` + visibleWhere(storedKindSQL(kindExpr), idExpr, authorIsOfRow(`visible_node`, param)) + `)`
}

// VisibleRow is the predicate over a `graph_nodes` row a read already holds, whatever its kind:
// the same test, with the comparison of the row's own kind (authorIsOfRow) and no second read of
// the table to make it. A read that walks the store's own table binds it (loadStoreNodes), and so
// does the count of an arm whose tuples come out of nodes of several kinds (liveVisibleNodeOf).
func VisibleRow(alias, param string) string {
	return VisibilityMarker + `the row's own kind */` + visibleHeldRow(alias, authorIsOfRow(alias, param))
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
	entry, held := nodeRegistry[kind]
	if !held {
		return "false"
	}
	return VisibilityMarker + kind + ` */` + visibleHeldRow(alias, entry.visibility.compares.authorIs(alias+`.made_by`, param))
}

// visibleHeldRow is the body both of those render: the row itself says whether the acting
// person may see the node, with no read of the table to find it. authorIs is the comparison of
// the row's author with the acting credential that its kind states.
func visibleHeldRow(alias, authorIs string) string {
	return `(` + alias + `.visibility <> '` + graphstore.VisibilityAuthor + `' or ` + authorIs + `)`
}

// visibleWhere is the body every rendered predicate shares: the store's row for the node, if it
// holds one, says whether the acting person may see it. authorIs is the comparison of that row's
// author, visible_node.made_by, with the acting credential that the node's kind states.
//
// It is a scalar subquery and not an `exists`, so the planner keeps it a sub-plan over the
// `graph_nodes` primary key rather than flattening it into an anti-join that reads the whole
// table: every read that binds this is keyed on an index of its own, and a count that gained a
// sequential scan of the store would be paid on every list.
func visibleWhere(kindExpr, address, authorIs string) string {
	return `coalesce((select visible_node.visibility <> '` + graphstore.VisibilityAuthor + `' or ` + authorIs + `
		from graph_nodes visible_node where visible_node.kind = ` + kindExpr + ` and visible_node.id = ` + address + `), true)`
}

// authorIsOfRow is the comparison of the author of the `graph_nodes` row alias names with the
// acting credential bound at param, by the row's own kind: the whole credential for a kind whose
// entry compares that (credentialComparedKinds), with a block's kind read off its id
// (graphstore.AddressKindSQL), and the author the credential proved for every other kind. While
// no kind compares the whole credential it is the one rule's comparison alone, with no test of
// the kind.
func authorIsOfRow(alias, param string) string {
	proved := graphstore.AuthorIsSQL(alias+`.made_by`, param)
	if len(credentialComparedKinds) == 0 {
		return proved
	}
	return `(case when ` + graphstore.AddressKindSQL(alias+`.kind`, alias+`.id`) + ` in (` + registrySQL.credentialCompared + `) then ` +
		graphstore.AuthorCredentialIsSQL(alias+`.made_by`, param) + ` else ` + proved + ` end)`
}

// quotedKinds is kinds as the SQL literals of an `in` list. Every kind is a constant of this
// build, never a value a request carried.
func quotedKinds(kinds []string) string {
	quoted := make([]string, len(kinds))
	for index, kind := range kinds {
		quoted[index] = `'` + kind + `'`
	}
	return strings.Join(quoted, ", ")
}
