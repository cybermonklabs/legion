package refs

import (
	"context"
	"maps"
	"slices"
	"strings"
	"sync"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// Every read of the graph answers only the nodes the acting person may see. A node carries that
// in `graph_nodes.visibility`: `project`, which everyone who may read the project reads and which
// every node of most kinds holds, or `author`, which reserves the node for whoever wrote it and
// which the store takes only for a kind the registry flags (nodeKind.authorOnly). A node
// reserved for its author is absent from the list of its kind, from the edges of every other
// node, from the backlink counts, from search, and from a read of its own `dispatch://` address,
// which answers exactly what an address nobody holds answers.
//
// The predicate of each standing kind - a lens, a finding of a lens, a risk, a constraint - also
// holds its node to not stale (StaleSQL): a lens whose recorded row no longer stands at its
// document's address, and every node whose part_of walk reaches one, is answered as a gone node
// is, absent from every list, chain and count and 404 NOT_FOUND by its own address. It rides in
// the kind's predicate rather than beside each read's test of `state <> 'gone'`, so no read of
// those kinds can leave it out. A predicate over a node whose kind is a column binds it only where
// that column can hold a standing kind (NodeKinds), since the planner charges it to every row the
// predicate filters, whatever that row's kind.
//
// The test is bound into the SQL of each read and never applied in Go, so a count and the list
// behind it cannot disagree: a row the count includes is a row the list shows.
//
// nodeRegistry (below) is the registry: one entry per node kind, carrying that kind's predicate
// beside the loader that reads its nodes, and every read binds its predicate through
// VisibleKind, VisibleEnd, VisibleNode, VisibleRow or VisibleKindRow rather than writing a test of
// its own. Every kind answers the same rule - the store's row for the node says whether the
// acting person may see it (visibleWhere) - and an entry states only what its kind's rule
// compares and the kind the store keys its nodes under (nodeVisibility). The loader and the
// predicate live in one value so a kind cannot be registered with a loader and no predicate: every
// constructor states one, and an entry written as a literal past the constructors is refused at
// init, where the kind is named (the three lines nodeKind states). Beside its rule, an artifact
// differs in how the from side of an edge spells it, which VisibleEnd rewrites
// (artifactStoreAddress).
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

// nodeKind is one node kind's entry in the registry below: the loader that reads the kind's
// nodes, and the predicate that decides which of them the acting person may see
// (nodeVisibility). The two are one value because a kind that resolves with no predicate is a
// read that answers a node its author reserved.
//
// Three lines keep a kind from reaching a read unguarded, and each one sees what the others
// cannot:
//
//   - The constructors below state the predicate, each the one its name says: resolvedKind and
//     unresolvedKind the rule most kinds take, the zero value of nodeVisibility, which compares
//     the author a credential proved and keys the store under the kind's own name, and
//     authoredKind and credentialAuthoredKind their own. No constructor makes an entry without a
//     predicate, so this is the line a kind added the way every kind here was added meets.
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
	// credentialAuthoredKind alone, the constructors of the kinds cybermonklabs/legion#4193 adds that
	// take `author` - interaction, lens, lens_block, risk and constraint - each called in the
	// commit that registers its kind; a lens_kind is always project-visible and takes neither.
	// The reads that leave a predicate out for a kind that can hold no such node read the store's
	// list back (authorOnlyGate).
	authorOnly bool
	// stated is true of an entry one of the constructors made, which is every entry whose
	// predicate someone stated: the zero value of nodeVisibility is a predicate in its own
	// right, the rule most kinds take, so the entry carries whether it was chosen rather than
	// defaulted. init refuses the registry over an entry whose stated is false.
	stated bool
}

// resolvedKind registers a node kind the graph addresses: the loader that reads its nodes, under
// the rule most kinds take (the zero value of nodeVisibility), which every read of the kind binds.
// The kind takes no node reserved for its author.
func resolvedKind(load nodeLoader) nodeKind {
	return nodeKind{load: load, stated: true}
}

// unresolvedKind registers a node kind the store holds rows of that no read of the graph
// resolves into a node. It answers the predicate resolvedKind states, like every other kind, and
// carries no loader.
func unresolvedKind() nodeKind {
	return nodeKind{stated: true}
}

// authoredKind registers a node kind that may hold a node reserved for its author and whose
// predicate compares the author a credential proved (comparesAuthor), the one rule every kind
// cybermonklabs/legion#3178 registers compares too: the loader that reads its nodes, every one of
// which binds that predicate. A lens, a block of a lens's finding (lens_block), a risk and a
// constraint are registered by it (the spec's R19), so a person's cookie, her
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
// every call of those predicates, which every read of the graph makes. Every kind this package
// writes into a statement as a literal (store.QuoteLiterals), here and in every other read, is a
// constant of this build, never a value a request carried.
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
		"issue":         resolvedKind(loadIssueNodes),
		"artifact":      resolvedKind(loadArtifactNodes),
		"ask":           resolvedKind(loadAskNodes),
		"comment":       resolvedKind(loadCommentNodes),
		"message":       resolvedKind(loadMessageNodes),
		"component":     resolvedKind(loadComponentNodes),
		"block":         resolvedKind(loadBlockNodes),
		"ask_block":     resolvedKind(loadAskBlockNodes),
		"comment_block": resolvedKind(loadCommentBlockNodes),
		"message_block": resolvedKind(loadMessageBlockNodes),
		// The kinds the node and tuple store holds itself (graphstore.KindPerson and its three
		// siblings, loadStoreNodes). None of them carries a `dispatch://` address, so the
		// artifact's address rewrite does not reach them.
		graphstore.KindPerson:  resolvedKind(loadStoreNodes),
		graphstore.KindSession: resolvedKind(loadStoreNodes),
		graphstore.KindRole:    resolvedKind(loadStoreNodes),
		graphstore.KindPage:    resolvedKind(loadStoreNodes),
		// The kinds of cybermonklabs/legion#4193 the store holds alone, each registered in the commit
		// that first writes a node of it (migration 20261123174710 writes the first two, the
		// reasoning write of api the next three, and the going POST of api the last). A lens kind
		// is a node of the catalogue, always project-visible, so its entry takes no node reserved
		// for its author; a constraint, a lens, a finding of a lens and a risk may each be reserved
		// for its author, and each one's predicate compares the author a credential proved, as
		// every kind cybermonklabs/legion#3178 registers does. A finding is stored as a node of kind
		// block at its own id, `lens/<uuid>#<block id>`, so its entry reads the store under that
		// kind (storedAsBlock). A lens, a finding and a risk are read by one loader, which takes the
		// kind it reads (loadReasoningNodes). An interaction, one run of the command bar, is
		// always reserved for its author, and its predicate compares the whole credential, as the
		// going stream compares its reader with the run's author.
		graphstore.KindLensKind:    resolvedKind(loadLensKindNodes),
		graphstore.KindConstraint:  authoredKind(loadConstraintNodes),
		graphstore.KindLens:        authoredKind(loadReasoningNodes),
		graphstore.KindLensBlock:   authoredKind(loadReasoningNodes).storedAsBlock(),
		graphstore.KindRisk:        authoredKind(loadReasoningNodes),
		graphstore.KindInteraction: credentialAuthoredKind(loadInteractionNodes),
		// The block schema's own nodes, which a type is (migration 20261109051232). Nothing
		// writes one outside a migration, every one of them is project-visible and no read of
		// the graph resolves one, so neither has a loader; but they are nodes of the store like
		// any other, so the registry answers for them rather than leaving the one read that lists
		// them (pmdoc/schemanodes, through graphstore.NodesOfKind) with no predicate to bind the
		// day a type is reserved for whoever drafted it.
		"supertag":  unresolvedKind(),
		"attribute": unresolvedKind(),
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
	registrySQL.kinds = store.QuoteLiterals(VisibilityKinds())
	registrySQL.credentialCompared = store.QuoteLiterals(credentialComparedKinds)
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
	// `made_by` records, the one rule of cybermonklabs/legion#3178 (graphstore.AuthorIsSQL): the
	// person a cookie, a personal token or a session the broker verified proves, else the
	// credential's own id, which counts for a session only where the broker proved that session
	// (model.Actor's Proven), else its workload. It is every kind cybermonklabs/legion#3178 registers,
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
	// storedKind is the kind `graph_nodes` keys the kind's nodes by, and is empty for every kind
	// the store keys under its own name. Only a finding of a lens differs: graph_edges and every
	// reference name it lens_block, at its own id `lens/<uuid>#<block id>`, and the store holds
	// it as a node of kind block at that same id (graphstore.KindLensBlock), so a predicate keyed
	// on lens_block would find no row and answer the finding to everyone (storedAsBlock).
	storedKind string
	// compares is what the predicate compares the author of a node reserved for that author
	// with: the author the acting credential proved, the zero value, which resolvedKind,
	// unresolvedKind and authoredKind state; or the whole acting credential, which
	// credentialAuthoredKind states.
	compares comparison
}

// storedAsBlock is the entry k with its nodes keyed in `graph_nodes` under BlockNodeKind rather
// than under the registry's own key (nodeVisibility.storedKind): a finding of a lens, registered
// as lens_block and stored as a block. Every predicate the registry renders for the entry reads
// the store's row under that kind, at the id the kind is addressed by.
func (k nodeKind) storedAsBlock() nodeKind {
	k.visibility.storedKind = BlockNodeKind
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

// ResolvesNodeKind is whether a read of the graph answers nodes of kind at all: the registry
// holds a loader for it (nodeKinds). A kind it does not resolve is the address of no node.
func ResolvesNodeKind(kind string) bool {
	return nodeRegistry[kind].load != nil
}

// ResolvableKinds is every node kind a read of the graph answers nodes of (ResolvesNodeKind),
// sorted (nodeKinds): the kinds of VisibilityKinds less those the registry holds a predicate for
// and no loader, the block schema's supertag and attribute. model.GraphNode's kind enum is these.
func ResolvableKinds() []string { return slices.Clone(nodeKinds) }

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
//
// A node of a standing kind - a lens, a finding, a risk, a constraint - that is stale (StaleSQL)
// is no node the person may see either, as a gone one is none any read answers: the predicate of
// each such kind holds the node to not stale, so no read of these kinds can answer one.
func VisibleKind(kind, address, param string) string {
	entry, held := nodeRegistry[kind]
	if !held {
		return "false"
	}
	return VisibilityMarker + kind + ` */(` +
		visibleWhere(`'`+StoredKindOf(kind)+`'`, address, entry.visibility.compares.authorIs(`visible_node.made_by`, param)) +
		` and ` + notStaleOfLiteralKind(kind, address) + `)`
}

// StoredNodeMarker opens the one read of `visibility` that is no test of it: the column a loader
// selects to answer a node's own stored visibility, beside its stored state (StoredRowOf). It
// decides nothing about which nodes a read answers, which the kind's predicate has already
// decided, so the census of the registry (TestEveryNodeKindAnswersAVisibilityPredicate) tells it
// from a predicate by this marker, and still holds every other `visibility` a statement reads to a
// predicate the registry rendered.
const StoredNodeMarker = "/* the node's stored state and visibility */"

// storedNodeAlias is the alias of the lateral join StoredNodeJoin adds to a statement's from list.
const storedNodeAlias = "stored_row"

// StoredState is the stored state StoredNodeJoin's join answers, as a statement beside that join
// names the column: null where the store holds no row of the node.
const StoredState = storedNodeAlias + ".stored_state"

// storedNode is what every loader of kind adds to its statement to answer the stored state and
// visibility of the node whose id address names, as `graph_nodes` keys it - the address that
// kind's predicate reads (VisibleKind): StoredNodeJoin of the kind the store keys kind by.
//
// It is a join that answers the row a read's own filters kept and nothing else: one more probe
// of the `graph_nodes` primary key for each node a read answers, beside the predicate's own probe
// of the same row. That is a choice for a loader that answers a handful of nodes. The block
// loader answers thousands - every block of an 8,001-block document, the 5,000 blocks a list of
// links points from - where the second probe cost a read of 5,001 blocks a fifth of its time
// (check c-read-cost), so it reads both from one probe (visibleStoredKind), as the loaders of an
// item's and a message's blocks do.
func storedNode(kind, address string) (join, columns string) {
	return StoredNodeJoin(`'`+StoredKindOf(kind)+`'`, address)
}

// visibleStoredKind is VisibleKind and storedNode of the node of kind whose id address names,
// read in one probe of the node's `graph_nodes` row rather than two, for a loader that answers
// many nodes at once: the block loader (loadBlockNodes), the loader of an item's blocks
// (itemBlockNodes) and the loader of a message's blocks (loadMessageBlockNodes). It answers the
// lateral join over that row the loader adds to its from list, after every relation address
// names; the two columns that answer storedNode's state and visibility from it (StoredRowOf),
// null where the store holds no row; and the predicate over it, VisibleKind's test of the same row
// under the same key with the same comparison of its author (VisibleKindRow), and true where the
// store holds no row, as VisibleKind reads such a node. So the loader answers the same nodes with
// the same state and visibility as the pair it replaces, and the join reads `visibility` once
// under each of the two markers, as that pair does.
//
// The join ends in `offset 0`, which keeps it a probe of the primary key per row, as VisibleKind's
// scalar sub-plan is (visibleWhere): a lateral subquery Postgres may pull up is a join it may hash
// from a scan of the whole store. A kind the registry does not hold joins nothing and answers
// false, as VisibleKind does.
func visibleStoredKind(kind, address, param string) (join, columns, predicate string) {
	if _, held := nodeRegistry[kind]; !held {
		return "", "null::text, null::text", "false"
	}
	const alias, row = "stored_visible", "stored_visible_node"
	join = `left join lateral (
			select ` + StoredRowOf(row) + `, ` + VisibleKindRow(kind, row, param) + ` as seen
			from graph_nodes ` + row + `
			where ` + row + `.kind = '` + StoredKindOf(kind) + `' and ` + row + `.id = ` + address + `
			offset 0
		) ` + alias + ` on true`
	return join, StoredColumnsOf(alias), `coalesce(` + alias + `.seen, true)`
}

// StoredNodeJoin is what a statement adds to answer the stored state and visibility of the node
// whose kind is the SQL kindExpr and whose id the SQL address names, as `graph_nodes` keys it: the
// lateral join to add to its from list, after every relation address names, and the two columns
// to select off it, the state and then the visibility the store holds for the node, each null
// where the store holds no row of it (StoredNode, setStored), the state named StoredState. Every
// loader of the registry but the loaders of blocks adds it for its own kind (storedNode), the
// loaders of blocks reading the same two columns with the kind's predicate in one probe of the
// row instead (visibleStoredKind), and a read outside this package that answers rows of several
// kinds - api's person page - adds it with the kind as its own SQL.
// It reads `visibility` once, opened by StoredNodeMarker.
func StoredNodeJoin(kindExpr, address string) (join, columns string) {
	return `left join lateral (select ` + StoredRowOf(`stored_node`) + ` from graph_nodes stored_node
		where stored_node.kind = ` + kindExpr + ` and stored_node.id = ` + address + `
		offset 0) ` + storedNodeAlias + ` on true`, StoredColumnsOf(storedNodeAlias)
}

// StoredRowOf is the stored state and the stored visibility of the `graph_nodes` row a statement
// holds as alias, as two columns named stored_state and stored_visibility, the visibility opened by
// StoredNodeMarker. A read that joins the node's row anyway selects these (api's lens read, the
// constraints of an issue), and so do the joins storedNode and visibleStoredKind add, so each reads
// that row once.
func StoredRowOf(alias string) string {
	return alias + `.state as stored_state, ` + StoredNodeMarker + alias + `.visibility as stored_visibility`
}

// StoredColumnsOf is the two columns StoredRowOf names, read off the relation alias that selected
// them: the stored state, then the stored visibility.
func StoredColumnsOf(alias string) string {
	return alias + `.stored_state, ` + alias + `.stored_visibility`
}

// StoredNode is the state and the visibility a read selected for one node (StoredRowOf), each
// null where the store holds no row of the node.
type StoredNode struct{ State, Visibility *string }

// StoredStateAndVisibility is the state and the visibility a read selected for a node, and live
// and project where the store holds no row of it, as VisibleKind reads such a node:
// `graph_nodes` is the store's own list, and a node no write has recorded there is as live and as
// project-visible as every node Dispatch wrote before the store existed.
func StoredStateAndVisibility(stored StoredNode) (state, visibility string) {
	state, visibility = graphstore.StateLive, graphstore.VisibilityProject
	if stored.State != nil {
		state = *stored.State
	}
	if stored.Visibility != nil {
		visibility = *stored.Visibility
	}
	return state, visibility
}

// setStored gives node the state and visibility storedNode read for it (StoredStateAndVisibility).
func setStored(node *model.GraphNode, stored StoredNode) {
	node.State, node.Visibility = StoredStateAndVisibility(stored)
}

// NodeKinds is what a read knows of the node an end of an edge names, for the predicate over that
// end (VisibleEnd), whose kind is a column of the read's own query: the kinds, as the graph
// addresses each (graphstore.AddressKind: a document's block is a block, a finding of a lens a
// lens_block), that column can hold (KindsOf), or every kind (AnyKind) where the end can be any
// node - the far end of a tuple. The predicate binds the staleness test (StaleSQL) only where one
// of those kinds is a standing kind, so a read whose ends can be none of a lens, a finding, a risk
// or a constraint carries none of it. A read that binds its predicates through the gate
// (authorOnlyGate) decides from the same kinds whether the end needs a predicate at all (gate).
//
// The test is gated on the row's kind (staleOfKindSQL), so over any other kind it reads nothing,
// but the planner costs a predicate by every branch it holds: it charges the test's recursive
// walk, which it reckons at ten steps, to every row the predicate filters, whatever that row's
// kind. Bound over the sources of an issue's references - asks, comments, messages and documents,
// where it never runs - it costed the closure walk over c-read-cost's corpus (64,106 blocks) at
// 985,892 where the same walk without it cost 74,308, past jit_above_cost and the inlining and
// optimisation thresholds, so Postgres compiled the walk's 553 expressions on every read: 11.8 s,
// past the walk's 2 s bound, where the walk itself reads in 0.1 s (cybermonklabs/legion#4193).
type NodeKinds struct {
	kinds []string
	any   bool
}

// AnyKind is an end that can be a node of any kind: the predicate over it tests the node for
// stale wherever its kind is a standing one.
var AnyKind = NodeKinds{any: true}

// KindsOf is an end that can be a node of one of kinds and of no other, each as the graph
// addresses it.
func KindsOf(kinds ...string) NodeKinds {
	return NodeKinds{kinds: slices.Clone(kinds)}
}

// mayBeStanding is whether an end of these kinds can be a node of a standing kind (standingKinds),
// which the predicate over it then tests for stale.
func (k NodeKinds) mayBeStanding() bool {
	return k.any || slices.ContainsFunc(k.kinds, func(kind string) bool { return slices.Contains(standingKinds, kind) })
}

// gate is what the gate (authorOnlyGate) decides in Go of the predicate over an end of these
// kinds: bind is whether a read binds it at all - whether the gate keeps the predicate of any
// kind the end can be (gateKeeps) - and inSQL whether the predicate goes behind the gate in SQL,
// tested on the end's own kind, which it need not where the gate keeps every one of those kinds'
// predicates. An end of any kind (AnyKind) binds it behind the gate in SQL.
func (k NodeKinds) gate() (bind, inSQL bool) {
	if k.any {
		return true, true
	}
	kept := 0
	for _, kind := range k.kinds {
		if gateKeeps(kind) {
			kept++
		}
	}
	return kept > 0, kept > 0 && kept < len(k.kinds)
}

// VisibleEnd is the predicate over an end of an edge, whose node kind is a column rather than a
// literal: one read of `graph_nodes` keyed on that column and on the id beside it, with the
// artifact's address rewrite (artifactStoreAddress) applied where the column says artifact, the
// kind read as the store keys it (storedKindSQL: an edge's lens_block is the store's block), and
// the comparison of the kind of the row it reads (authorIsOfRow). kinds is what the read knows
// the column can hold (NodeKinds): an end that can be a node of a standing kind is held to not
// stale (staleOfKindSQL), as VisibleKind holds one, and any other end is not tested at all.
// byRefKey says the ids are the ones the to side of an edge carries, which are already the ids
// the store keys a node by; the from side carries an artifact's uuid instead, and the rewrite
// translates it.
//
// A kind the registry does not hold passes: such an end resolves to no node and the read drops
// its edge anyway (resolveNodes), and refusing here would refuse the whole read for one row a
// newer build wrote.
func VisibleEnd(kinds NodeKinds, kindExpr, idExpr string, byRefKey bool, param string) string {
	return visibleEnd(kinds, kindExpr, idExpr, byRefKey, param, false)
}

// visibleEnd is VisibleEnd, with its read of `graph_nodes` behind the gate (authorOnlyGate),
// tested on the end's own kind, where gated says so (gatedEnd). The staleness test stays
// beside the gate.
func visibleEnd(kinds NodeKinds, kindExpr, idExpr string, byRefKey bool, param string, gated bool) string {
	address := idExpr
	if !byRefKey {
		address = `case ` + kindExpr + ` when 'artifact' then ` + artifactStoreAddress(idExpr) + ` else ` + idExpr + ` end`
	}
	stale := ""
	if kinds.mayBeStanding() {
		stale = ` and not ` + staleOfKindSQL(kindExpr, idExpr)
	}
	visible := visibleWhere(storedKindSQL(kindExpr), address, authorIsOfRow(`visible_node`, param))
	if gated {
		visible = authorOnlyGate(kindExpr, idExpr, visible)
	}
	return VisibilityMarker + `every kind */(` + visible + stale + `)`
}

// gatedEnd is VisibleEnd's predicate over an end of kinds, for a read that binds it through the
// gate (authorOnlyGate), and true, reading nothing, where the gate keeps the predicate of no kind
// the end can be: VisibleEnd whole where it keeps every one's, and otherwise VisibleEnd with its
// read of `graph_nodes` behind the gate in SQL (NodeKinds.gate), the graph read's end of any kind
// (AnyKind) among them.
func gatedEnd(kinds NodeKinds, kindExpr, idExpr string, byRefKey bool, param string) string {
	bind, inSQL := kinds.gate()
	if !bind {
		return "true"
	}
	return visibleEnd(kinds, kindExpr, idExpr, byRefKey, param, inSQL)
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
// A node the store holds no row for is visible, as VisibleKind reads one. A stale node of a
// standing kind is not (staleOfKindSQL), as VisibleKind holds one.
func VisibleNode(kindExpr, idExpr, param string) string {
	return VisibilityMarker + `the row's own kind */(` + graphstore.AddressKindSQL(kindExpr, idExpr) + ` in (` +
		registrySQL.kinds + `) and ` + visibleWhere(storedKindSQL(kindExpr), idExpr, authorIsOfRow(`visible_node`, param)) +
		` and not ` + staleOfKindSQL(kindExpr, idExpr) + `)`
}

// VisibleRow is the predicate over a `graph_nodes` row a read already holds, whatever its kind:
// the same test, with the comparison of the row's own kind (authorIsOfRow) and no second read of
// the table to make it. A read that walks the store's own table binds it (loadStoreNodes), and so
// does the count of an arm whose tuples come out of nodes of several kinds (liveVisibleNodeOf). A
// stale row of a standing kind is no row it admits (staleOfKindSQL), as VisibleKind holds one.
func VisibleRow(alias, param string) string {
	return VisibilityMarker + `the row's own kind */(` + visibleHeldRow(alias, authorIsOfRow(alias, param)) +
		` and not ` + staleOfKindSQL(alias+`.kind`, alias+`.id`) + `)`
}

// VisibleGoneRow is the predicate over a `graph_nodes` row a read already holds that every read
// answers as gone (GoneOrStaleRow), for a reader the row's own kind admits but for that: its state
// is gone, or it is a stale node of a standing kind (StaleSQL), and the comparison of its own kind
// (authorIsOfRow) admits the reader as VisibleRow's does. The one read of it is a write's, which
// refuses a write on a node it finds so 409 NODE_GONE (api's findWrittenNode), and answers a node
// the reader may not see, gone or not, with the 404 every read answers it.
func VisibleGoneRow(alias, param string) string {
	return GoneOrStaleRow(alias) + ` and ` +
		VisibilityMarker + `the row's own kind */` + visibleHeldRow(alias, authorIsOfRow(alias, param))
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
// is there, and a read binds this only where it answers nothing for a node that has none. A stale
// row of a standing kind is no row it admits, as VisibleKind holds one.
func VisibleKindRow(kind, alias, param string) string {
	entry, held := nodeRegistry[kind]
	if !held {
		return "false"
	}
	return VisibilityMarker + kind + ` */(` + visibleHeldRow(alias, entry.visibility.compares.authorIs(alias+`.made_by`, param)) +
		` and ` + notStaleOfLiteralKind(kind, alias+`.id`) + `)`
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

// storeAdmitsAuthor is the kinds the store takes a node reserved for its author of
// (graphstore.AuthorOnlyKinds), the kinds this registry flags (nodeKind.authorOnly) and hands it at
// init (graphstore.AdmitAuthorOnly), and those kinds as the literals of an `in` list, read once, by
// the first read that gates a predicate on them (authorOnlyGate, gateKeeps): after every init has
// run, so after this registry's own has handed them over. The gate reads the store's own list and
// keeps none of its own, so the gate and the store's refusal are one definition and cannot drift
// apart.
var storeAdmitsAuthor = sync.OnceValues(func() ([]string, string) {
	kinds := graphstore.AuthorOnlyKinds()
	return kinds, store.QuoteLiterals(kinds)
})

// authorOnlyGate is visible, a test of whether the acting person may see the node whose kind and
// id are the SQL kindExpr and idExpr, made only where that node can be reserved for its author:
// true, reading nothing, of a node whose kind as the graph addresses it
// (graphstore.AddressKindSQL: a block whose id names a lens is a lens_block) is none of the kinds
// the store admits `author` for (storeAdmitsAuthor), and visible of every other node.
//
// It answers what visible answers, row for row, because the store refuses `author` on a node of
// every other kind before any node write (graphstore.CheckVisibilityAdmitted): visible holds of
// every such node there is, so the gate spares the read visible's probe of `graph_nodes` for each
// of them and changes no answer. This is the one statement of that reason, and every read that
// binds a predicate only where its node can be reserved for its author binds it through this gate:
// in SQL, here, for a node whose kind is a column of the read (gatedEnd), and in Go for one
// whose kind is a literal of the build or a column of kinds the read knows (gateKeeps,
// NodeKinds.gate). The staleness test of a standing kind (staleOfKindSQL) is no visibility test,
// and no part of the gate: every caller keeps it beside the gate.
func authorOnlyGate(kindExpr, idExpr, visible string) string {
	_, admitted := storeAdmitsAuthor()
	return `(` + graphstore.AddressKindSQL(kindExpr, idExpr) + ` not in (` + admitted + `) or ` + visible + `)`
}

// gateKeeps is the gate (authorOnlyGate) decided in Go, for the predicate of kind, a kind of the
// registry a read writes as a literal or knows a column to hold (NodeKinds): whether a read that
// binds its predicates through the gate binds kind's.
//
// It keeps the predicate wherever a row the predicate reads can be one the store admitted
// `author` on. The predicate reads the store's row under the kind the store keys kind by
// (StoredKindOf), at whatever id the read names, and the store addresses that row by that stored
// kind, or by lens_block where it is a block whose id names a lens (graphstore.AddressKind),
// which the kind alone cannot tell: so a document's block keeps its predicate, since the store
// keys a finding as a block. It keeps too the predicate of a kind the registry does not hold,
// which answers false and so is no test the gate may spare, and the predicate of a standing kind,
// which holds its node to not stale (notStaleOfLiteralKind): every standing kind is one the store
// admits `author` for, and this keeps that test bound should one ever not be.
func gateKeeps(kind string) bool {
	if _, held := nodeRegistry[kind]; !held || slices.Contains(standingKinds, kind) {
		return true
	}
	stored := StoredKindOf(kind)
	admitted, _ := storeAdmitsAuthor()
	return slices.ContainsFunc(admitted, func(other string) bool { return StoredKindOf(other) == stored })
}

// gatedKind is VisibleKind's predicate of kind, a literal of the build, at address, where the
// gate keeps it (gateKeeps), and true, reading nothing, where it does not: a node of a kind the
// store reserves for nobody is one every reader may see (authorOnlyGate).
func gatedKind(kind, address, param string) string {
	if !gateKeeps(kind) {
		return "true"
	}
	return VisibleKind(kind, address, param)
}
