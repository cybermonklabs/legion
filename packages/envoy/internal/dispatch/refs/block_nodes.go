package refs

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/rank"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// blockNodeAddress is a block's address in the store, which document_blocks.node_id and
// block_refs.from_node hold and which the graph addresses a block by: the document's ref_key,
// '#' and the block's id as the block index keys it (storedBlockID). An id read from the store,
// or from the rendering the index is written from, is already in that form.
func blockNodeAddress(refKey, blockID string) string { return refKey + "#" + blockID }

// BlockAddress is the store's address of block blockID of the document whose ref_key is refKey,
// for an id that may not be in the form the block index keys it - one a request posted, or one a
// caller read off a typed block - read as the index keys it (storedBlockID). It is the one
// address a reader outside this package names a block's node by, so the Keep's lookup of a
// decision and the restore's moved_to tuples name the node the index wrote.
func BlockAddress(refKey, blockID string) string {
	return blockNodeAddress(refKey, storedBlockID(blockID))
}

// storedBlockID is a block's id as the block index keys it: a U+0000 reads as U+FFFD, as the
// rendering writes it (pmdoc.BlockPointers).
func storedBlockID(blockID string) string { return strings.ReplaceAll(blockID, "\x00", "\uFFFD") }

// MaxBlockNodeBytes bounds a block's address in the store, which is one address of the store:
// graph_nodes keys a node by (kind, id) in a B-tree, and graph_tuples_once, the repeat key of a
// relation, keys both of its ends at once, so a block's address and the far end of every tuple
// out of it have to fit the roughly 2,704 bytes a B-tree entry holds. A block's address is one
// of two ends, and its far end is not a second block address but a reference target of up to
// text.MaxTargetBytes (a mention's far end) or another node's address: the store bounds each end
// at graphstore.MaxAddressBytes, so two ends, the two kinds and the source_event and asserted the
// key also carries stay well under the entry bound. A block's address holds its document's
// ref_key, which nothing short of that key's own unique index bounds (migration 0121 hash-indexes
// document_blocks.node_id for the same reason), so a long-named document's blocks can pass the
// bound on their own.
//
// It is the store's one address bound and is defined there (graphstore.MaxAddressBytes): the
// census of the store against its projections and the backfill of the rows written before the
// store existed (internal/dispatch/graphcensus) read the same bound, and a block past it carries
// no node here, so a census holding such a row to one would report a difference this writer made
// on purpose.
const MaxBlockNodeBytes = graphstore.MaxAddressBytes

// blockIDFits is whether the address a block's id would take is inside that bound, read off
// the two lengths that make the address rather than off the address itself: a pass over a
// document's blocks asks this of every block and writes the address of the few it states
// something about, so building a string per block here would be a string per block thrown
// away.
func blockIDFits(refKey, blockID string) bool {
	return len(refKey)+len("#")+len(blockID) <= MaxBlockNodeBytes
}

// blockFits is whether each block's address fits the store's key, and how many do not.
func blockFits(refKey string, nodes []model.ArtifactBlock) ([]bool, int) {
	fits := make([]bool, len(nodes))
	past := 0
	for index, block := range nodes {
		fits[index] = blockIDFits(refKey, block.ID)
		if !fits[index] {
			past++
		}
	}
	return fits, past
}

// blockNode is one block's node as a version's index writes it: project-visible, in state, in the
// project of the document that holds it, made by maker at at, and with no cause yet -
// graphstore.Caused gives it the version's event. A node already there keeps its made_at and
// made_by, so a block is made by the version that first held it: the first of that version's
// authors at its time (versionAuthors), graph_nodes.made_by holding one actor. A block an earlier
// build indexed with no project takes its document's here (graphstore.Write fills a project the
// row does not hold). supertag is the supertag of the block's type (TypedFacts.supertag): its type
// for a typed block, BlockSupertag for every other kind and for a reading in an ask nobody has
// answered, so the typed-node pass and this index write one node and neither moves what the other
// wrote.
//
// The index owns a block's text and its place in its document, not its state or its visibility,
// so every node it writes restates the block (graphstore.Node's Restates): state is live for a
// block the version holds and gone for one it dropped, and nothing more. A block already there
// keeps its visibility, and a block the version holds keeps the state a write marked it with -
// a proposed requirement whose words a version changes stays proposed - while one stored gone,
// a block a version restores, comes back live. A block the version dropped goes gone whatever
// it was.
//
// changed marks a block whose words this version added, changed or restored, or which it
// dropped, so a version written for a run names that block as changed by it (graphstore.Node's
// Changed); a block the pass writes only because its row names no node yet is not marked.
func blockNode(address string, record IndexedVersion, maker model.Actor, at time.Time, state, supertag string, changed bool) graphstore.Node {
	return graphstore.Node{
		Kind: BlockNodeKind, ID: address, Supertag: supertag,
		Visibility: graphstore.VisibilityProject, State: state,
		Project: record.Project, Parties: record.Parties,
		MadeAt: at, MadeBy: maker, SourceEvent: graphstore.NoEvent,
		Restates: true, Changed: changed,
	}
}

// top is the node a top-level block of the document hangs under: the issue's node for a block of
// an issue's spec, Verification or plan document (and, once the issue room exists, of its tree's
// own fragment), so one walk from the issue reaches every node of it, and the document's own node
// for an upload or a project document, whose body is that document's. document_blocks keeps a
// null parent_block_id for either, and in_document still says which document the block is in.
func (record IndexedVersion) top() (kind, id string) {
	if record.Band != RankBandOther && record.Issue != "" {
		return graphstore.KindIssue, record.Issue
	}
	return graphstore.KindArtifact, record.RefKey
}

// topOf is the node block hangs under at the top level: for a block of an issue room that another
// document's fragment holds (fragmentDocuments), that document's top - the issue's node for a
// block of the issue's spec, Verification or plan document, and the document's own node for an
// upload's, as the store census reads a row's top off its document (DocumentTopSQL over
// BlockDocumentSQL) - and the indexed document's own top for every other block.
func (record IndexedVersion) topOf(block model.ArtifactBlock, fragments map[string]fragmentDocument) (kind, id string) {
	if held, ok := fragments[block.Document]; ok {
		return IndexedVersion{RefKey: held.refKey, Issue: record.Issue, Band: held.band}.top()
	}
	return record.top()
}

// The bands a block's rank stands in (D15 of the issue-tree plan): its first byte says
// which document the block is in, and the rest is rank.Between inside that band. So under one
// parent the spec's blocks come first, then the tree's own fragment's, then the Verification
// document's, then the plan's, then any other document's, each in its own document order, and the
// pass over one document never moves another's ranks: the issue's first child is the spec's first
// top-level block whatever the other documents hold, which is what makes the spec's memo the
// issue's first child. RankBandTree is the band of the issue room's own fragment: the blocks the
// issue's tree artifact holds itself. A block of an issue room that another document's fragment
// holds stands in that document's band (model.ArtifactBlock's Document, blockBands).
const (
	RankBandSpec         byte = '1'
	RankBandTree         byte = '2'
	RankBandVerification byte = '3'
	RankBandPlan         byte = '4'
	RankBandOther        byte = '5'
)

// DocumentBandSQL is the band of the artifacts row aliased alias as one character of SQL text, the
// one statement of which document plays which part that the block index ranks by and the census
// reads the ranks back against: an issue's tree artifact is the issue room's own fragment; an
// issue's primary document is its spec; an issue's markdown document whose role is Verification or
// plan, or that holds no role and carries that role's slug (a row a rolling deploy left with none,
// or one the issue room's conversion cleared), is that document; and every other document - an
// upload, a project's document - stands in the last band. DocumentBand is the same rule over the
// fields of a row a transaction has queued and not written, which no statement reads yet.
func DocumentBandSQL(alias string) string {
	a := alias + "."
	role := func(role, slug string) string {
		return `(` + a + `role = '` + role + `' or (` + a + `role is null and ` + a + `slug = '` + slug + `'))`
	}
	return `(case when ` + a + `issue_key is not null and ` + a + `kind = 'tree' then '` + string(RankBandTree) + `'
		when ` + a + `issue_key is null or ` + a + `kind <> 'doc' then '` + string(RankBandOther) + `'
		when ` + a + `is_primary then '` + string(RankBandSpec) + `'
		when ` + role(model.ArtifactRoleVerification, model.ArtifactSlugVerification) + ` then '` + string(RankBandVerification) + `'
		when ` + role(model.ArtifactRolePlan, model.ArtifactSlugPlan) + ` then '` + string(RankBandPlan) + `'
		else '` + string(RankBandOther) + `' end)`
}

// DocumentBand is DocumentBandSQL over an artifacts row's issue_key, kind, is_primary, role and
// slug, nil for a null issue_key or role: the band of a document whose row the writing transaction
// has queued (docs' indexedVersionOf, refs.DocumentAddress's Band).
func DocumentBand(issueKey *string, kind string, primary bool, role *string, slug string) byte {
	holds := func(want, wantSlug string) bool {
		return role != nil && *role == want || role == nil && slug == wantSlug
	}
	switch {
	case issueKey != nil && kind == "tree":
		return RankBandTree
	case issueKey == nil || kind != "doc":
		return RankBandOther
	case primary:
		return RankBandSpec
	case holds(model.ArtifactRoleVerification, model.ArtifactSlugVerification):
		return RankBandVerification
	case holds(model.ArtifactRolePlan, model.ArtifactSlugPlan):
		return RankBandPlan
	}
	return RankBandOther
}

// DocumentTopSQL is the far end a top-level block of the document of the artifacts row aliased
// alias hangs under, written kind:id as the census writes a link (IndexedVersion.top): the
// issue's node for a document of a band other than RankBandOther, which only an issue's document
// takes, and the document's own node for every other.
func DocumentTopSQL(alias string) string {
	return `(case when ` + DocumentBandSQL(alias) + ` <> '` + string(RankBandOther) + `' then '` + graphstore.KindIssue + `:' || ` +
		alias + `.issue_key else '` + graphstore.KindArtifact + `:' || ` + alias + `.ref_key end)`
}

// BlockDocumentSQL is the document the document_blocks row aliased alias is a block of, as SQL
// text answering a uuid: the document its issue_room_blocks row names, for a block of an issue room
// (a row keyed by the room's tree artifact, whose fragment's document that row records: T12a of
// the issue-tree plan), else the row's own artifact_id. Every reader that takes a row's
// artifact as the block's document - to find its role, issue, slug or live blocks, or to map the
// version it records to that document's versions, their times and events, its indexed versions
// or its keeps - takes it from here, and keys a version by this document and the number: a
// room's rows carry each document's own version numbers under one key. A block's node address,
// and every pointer at it, keep the row's own artifact.
func BlockDocumentSQL(alias string) string {
	return BlockDocumentOrSQL(alias, alias+".artifact_id")
}

// BlockDocumentOrSQL is BlockDocumentSQL with fallback, SQL text answering a uuid, in place of the
// row's own artifact where no issue_room_blocks row names the block (graphstore.RoomBlockRowSQL):
// a reader that found a block's node in a document of an issue room before the room's block index
// reached it - a document that owes that index, whose typed nodes the typed-node pass placed in it
// (PlanDecisionNodes) - passes that document, which is the only record of it, and the row's own
// artifact after it.
func BlockDocumentOrSQL(alias, fallback string) string {
	return `coalesce(` + graphstore.RoomBlockRowSQL(alias+".artifact_id", alias+".block_id") + `, ` + fallback + `)`
}

// retiredDocumentSQL is true while the document the uuid SQL expression document names is retired
// (artifacts.retired_at), and false for a null document. Over the document of a block of an issue
// room (graphstore.RoomBlockRowSQL, RoomBlockDocumentSQL), which is null for a block of no room, it
// is true of a block of a retired document of the room: that document's fragment leaves the room's
// layout (docs' roomLayoutSQL), and the room's passes keep its blocks as its retirement left them
// (BlockIndexing.retained).
func retiredDocumentSQL(document string) string {
	return `exists (select 1 from artifacts retired where retired.id = ` + document + ` and retired.retired_at is not null)`
}

// RoomBlockDocumentSQL is the document whose fragment of the issue room under the tree artifact
// tree (SQL text answering a uuid) holds, or last held, the block block (SQL text), as SQL text
// answering a uuid, null where none is on record: the document its issue_room_blocks row names
// (graphstore.RoomBlockRowSQL) while the room holds the block, else, for a block a version dropped
// from the room, whose row went with it, the document the last asserted in_document of its node,
// `<tree ref_key>#<block>`, names, since the block index states a block's membership of each
// document it moves into and retracts none when the block is dropped. An artifact that is no
// issue's tree holds no record here. Every reader that takes the document of a room's block that
// may have left the room - an ask's block (AskBlockDocumentSQL), a block a write's event names
// (CountedChanges) - takes it from here.
func RoomBlockDocumentSQL(tree, block string) string {
	// The node's address is read first, behind a fence, and the tuples are read in a lateral keyed
	// on it, so they are probed on the node (from_id) and never scanned by kind. An in_document
	// always runs to an artifact (the join below reads it), so the probe names no to_kind, which
	// would lead the planner to the inbound indexes.
	return `coalesce(` + graphstore.RoomBlockRowSQL(tree, block) + `,
		(select stood.id
		 from (select room_tree.ref_key || '#' || ` + block + ` as address from artifacts room_tree
		       where room_tree.id = ` + tree + ` and room_tree.kind = 'tree' offset 0) node
		 cross join lateral (
		   select stood_in.to_id from graph_tuples stood_in
		   where stood_in.from_kind = '` + BlockNodeKind + `' and stood_in.from_id = node.address
		     and stood_in.kind = '` + TupleInDocument + `' and stood_in.asserted
		   order by stood_in.seq desc limit 1
		 ) stood_in
		 join artifacts stood on stood.ref_key = stood_in.to_id))`
}

// DocumentRoomBlocksSQL is true of the document_blocks row aliased alias while it is a block of
// the document whose uuid the SQL expression document names, read where the block index keys it:
// a row of the room that document is edited in (DocumentRoomSQL) whose document is that one
// (BlockDocumentSQL), the document's own rows where it is its own room. Every reader that takes a
// document's id and reads its block rows reads them through it, never by the document's own id:
// an issue born converted keeps its documents' rows under its tree artifact.
func DocumentRoomBlocksSQL(alias, document string) string {
	return alias + `.artifact_id = ` + DocumentRoomSQL(document) + ` and ` + BlockDocumentSQL(alias) + ` = ` + document
}

// AskBlockDocumentSQL is the document holding the block of the ask aliased alias, as SQL text
// answering a uuid: an ask of an issue room is keyed by the room's tree artifact (its
// block_artifact_id), and its block is in the document whose fragment the room records it in
// (RoomBlockDocumentSQL: its issue_room_blocks row, a block past text.MaxBlockIDBytes included, or
// for a block gone from the room its node's last in_document); every other ask's block is in its
// block_artifact_id. Every reader that takes an ask's block document - to show it, to look a
// document's asks up, or to write into the fragment holding the block - takes it from here, and
// looks a document's asks up under that document's room (DocumentRoomSQL).
func AskBlockDocumentSQL(alias string) string {
	return `coalesce(` + RoomBlockDocumentSQL(alias+".block_artifact_id", alias+".block_id") + `, ` + alias + `.block_artifact_id)`
}

// AsksOfDocumentSQL is true of the ask aliased alias while its block is in the document the uuid
// SQL expression document names: an ask keyed by the room that document is edited in
// (DocumentRoomSQL) whose block that document holds (AskBlockDocumentSQL). Every read of a
// document's asks by the document's id reads them so, for a document of an issue room and for one
// that is its own room alike; a room's settlement, which reconciles every fragment at once, reads
// its room's asks by the room's key.
func AsksOfDocumentSQL(alias, document string) string {
	return `(` + alias + `.block_artifact_id = ` + DocumentRoomSQL(document) + ` and ` + AskBlockDocumentSQL(alias) + ` = ` + document + `)`
}

// CheckResultsOfDocumentSQL is true of the check_results row aliased alias while it is a result of
// a check of the document the uuid SQL expression document names: a row keyed by that document,
// which a document that is its own room, and every result recorded before the issue room, holds;
// or a row keyed by the room that document is edited in (DocumentRoomSQL), the tree artifact of an
// issue room, whose block that document holds or last held (RoomBlockDocumentSQL). A result's row
// is keyed by the artifact its check's document_blocks row is keyed by (T11 of the issue-tree plan),
// so every read of a document's results by the document's id reads them so, never by the row's key
// alone.
func CheckResultsOfDocumentSQL(alias, document string) string {
	return `(` + alias + `.check_artifact_id = ` + document + ` or (` + alias + `.check_artifact_id = ` + DocumentRoomSQL(document) +
		` and ` + RoomBlockDocumentSQL(alias+".check_artifact_id", alias+".check_block_id") + ` = ` + document + `))`
}

// AsksOfFragmentBlocksSQL is true of the ask aliased alias while it is the ask of one of the
// blocks under the ids the text[] SQL expression blocks names, read off the document's own
// fragment (its live tree or one of its versions), in the room the document the uuid SQL
// expression document names is edited in (DocumentRoomSQL). Block ids are unique across a room,
// so the room and the fragment's own ids name the document's asks whether or not the room's index
// holds those blocks yet, which AsksOfDocumentSQL reads.
func AsksOfFragmentBlocksSQL(alias, document, blocks string) string {
	return `(` + alias + `.block_artifact_id = ` + DocumentRoomSQL(document) + ` and ` + alias + `.block_id = any(` + blocks + `))`
}

// DocumentRoomSQL is the room the document whose id the uuid SQL expression document names is
// edited in, as SQL text answering a uuid: its document_rooms row's room, for a document converted
// into its issue's room, else the document itself. It is the one SQL of that resolution: docs'
// roomOf (DocumentRoomOfStatement) reads it, as every statement that needs a document's room
// beside its other work does.
func DocumentRoomSQL(document string) string {
	return `coalesce((select moved.room from document_rooms moved where moved.artifact_id = ` + document + `), ` + document + `)`
}

// DecisionsQuerySQL is true of the artifacts row aliased alias while it is its issue's decisions
// query node (api's createDecisionsQuery, D6 of the issue-tree plan): a document whose node is a
// query node of query `decisions` (graphstore.QueryNode) and which no document_rooms row joins to
// an issue room, whose versions render that query. No fragment of any room holds it, so no socket
// opens on it (docs.Service.ServeHTTP), no upload or edit writes it (api's decisionsQueryRefusal),
// and it is no backlink of its issue (issueOwnArtifact). It is the one SQL of that test, kept in
// refs, which docs and api both import, so that every reader of it reads this one. Its probe of
// document_rooms ends in `offset 0`, which keeps it a probe of that table's key per row: without
// it Postgres may read the probe as a hashed subplan over the whole table, as the issue's backlink
// count did under its generic plan once its attached_to arms bound no predicate of their own
// (TestBacklinkCountStatementReadsEveryArmThroughAnIndex).
func DecisionsQuerySQL(alias string) string {
	return `(` + alias + `.kind = 'doc' and not exists (select 1 from document_rooms moved where moved.artifact_id = ` + alias + `.id offset 0)
		and exists (select 1 from graph_nodes node where node.kind = '` + graphstore.KindArtifact + `' and node.id = ` + alias + `.ref_key
		            and node.supertag = '` + graphstore.SupertagQuery + `'
		            and node.attributes->>'` + graphstore.AttributeQuery + `' = '` + graphstore.QueryDecisions + `'))`
}

// ResolvedBlockSQL is the node a block far end of a reference resolves to, as SQL text answering a
// node id, null where it resolves to none; address is the far end, a text SQL expression holding
// a node id (text.TargetID). The one resolution is block_moved_to's (migration
// 20261118165901_issue_tree_block_moved_to_room, T17 of the issue-tree plan): the address's current
// moved_to tuple first, followed hop by hop, then the room its document's document_rooms row
// names, then the address itself. A probe in front of it answers an address a document_blocks row
// holds where no moved_to tuple runs from it and its document has no room, which is what the
// function answers there, so a block that stands in a document that is its own room costs three
// index probes and no call of the function, and a statement reading it reaches document_blocks
// through document_blocks_node_id. Every statement that resolves a far end reads it from here
// (BlockLinks, Closure, BlockTargets, ResolveFarEnds), and graphstore.MovedTo calls the function
// alone.
func ResolvedBlockSQL(address string) string {
	return `coalesce((select held.node_id from document_blocks held
		where held.node_id = ` + address + `
		  and not exists (select 1 from document_rooms room where room.artifact_id = held.artifact_id)
		  and not exists (select 1 from graph_tuples moved where moved.from_kind = '` + BlockNodeKind + `'
		    and moved.from_id = ` + address + ` and moved.kind = '` + graphstore.TupleMovedTo + `')
		limit 1), block_moved_to(` + address + `))`
}

// UnheldBlockSQL is the node address a block far end names without asking for a row, as SQL text
// answering a node id, null where its moved_to tuples run in a cycle or past their bound: the
// address its current moved_to tuples reach (block_address_walk), the same block id in the room
// its document's document_rooms row names where the document has one (block_room_address), the
// two steps ResolvedBlockSQL takes before it asks for a document_blocks row. It is where a far end
// whose block a version deleted stood: the block index's writer keeps a pointer at such a block
// there (resolvePointerPairs), and a reader naming the node of a block no current version holds
// reads it through NamedBlockSQL.
func UnheldBlockSQL(address string) string {
	return `(select coalesce(block_room_address(walked.reached), walked.reached)
		from (select block_address_walk(` + address + `) as reached) walked)`
}

// NamedBlockSQL is the node a block far end names whether or not a block stands there, as SQL
// text answering a node id: the node it resolves to (ResolvedBlockSQL) where one does, else the
// address the resolution's two steps name without asking for a row (UnheldBlockSQL), else the far
// end itself. So the address of a block a version deleted from a document converted into its
// issue's room names the room's node the block stood at, whose state and facts the store still
// holds, as the address of such a block of a document that is its own room names its own node.
func NamedBlockSQL(address string) string {
	return `coalesce(` + ResolvedBlockSQL(address) + `, ` + UnheldBlockSQL(address) + `, ` + address + `)`
}

// NamedBlock is the node one block far end names (NamedBlockSQL), read in one statement that
// calls block_moved_to alone where NamedBlockSQL probes document_blocks first, as
// graphstore.MovedTo reads one address: the probe answers what the function answers, and is
// there for a statement over many far ends.
func NamedBlock(ctx context.Context, q Queryer, address string) (string, error) {
	var node string
	if err := q.QueryRow(ctx, `select coalesce(block_moved_to($1::text), `+UnheldBlockSQL("$1::text")+`, $1::text)`,
		address).Scan(&node); err != nil {
		return "", fmt.Errorf("read the node %s names: %w", address, err)
	}
	return node, nil
}

// ResolveFarEnds reads, in one statement, the node each of addresses (block far ends, node ids)
// resolves to (ResolvedBlockSQL), keyed by the address, for each that resolves to another node
// than itself: an address that resolves to itself or to no block is absent. It answers no reader:
// a read whose answer a reader is shown resolves through ReadableFarEnds.
func ResolveFarEnds(ctx context.Context, q Queryer, addresses []string) (map[string]string, error) {
	return resolveFarEnds(ctx, q, nil, addresses)
}

// ReadableFarEnds is ResolveFarEnds for the reader who: an address resolves only to a node who may
// see, its statement binding the block predicate of the registry's (VisibleKind) over the node it
// resolves to, so a block the reader may not see is not reached through an address that moved to
// it or through its document's room, and the address stays as written, naming what it names for
// that reader.
func ReadableFarEnds(ctx context.Context, q Queryer, who model.Actor, addresses []string) (map[string]string, error) {
	return resolveFarEnds(ctx, q, &who, addresses)
}

// resolveFarEnds is ResolveFarEnds for who, or for no reader where who is nil.
func resolveFarEnds(ctx context.Context, q Queryer, who *model.Actor, addresses []string) (map[string]string, error) {
	if len(addresses) == 0 {
		return nil, nil
	}
	var args queryArgs
	list := args.param(addresses)
	query := `
		select p.address, resolved.node_id
		from unnest(` + list + `::text[]) as p (address)
		cross join lateral (select ` + ResolvedBlockSQL("p.address") + ` as node_id) resolved
		where resolved.node_id <> p.address` + args.andVisible(who, BlockNodeKind, "resolved.node_id")
	rows, err := q.Query(ctx, query, args.values...)
	if err != nil {
		return nil, fmt.Errorf("resolve the far ends %v: %w", addresses, err)
	}
	defer rows.Close()
	resolved := make(map[string]string)
	for rows.Next() {
		var address, node string
		if err := rows.Scan(&address, &node); err != nil {
			return nil, fmt.Errorf("scan a resolved far end: %w", err)
		}
		resolved[address] = node
	}
	return resolved, rows.Err()
}

// MovedRoomBlockSQL is true of the block block (SQL text) of the artifact artifact (SQL text
// answering a uuid) while that artifact is an issue's tree and the block's node has stood in more
// than one document of its room: its in_document tuples, asserted at any time, name more than one
// (a block cut from one fragment and pasted into another, T12a of the issue-tree plan).
// Such a block's document_block_texts rows are keyed by the tree and each records a version of
// the document that held the block when its pass wrote it, and no row names that document, so
// which document's version a row records is known of no row: the version that first held the
// block cannot be read off them (blockFirstVersion). A block of any other artifact never changes
// document and is never one.
func MovedRoomBlockSQL(artifact, block string) string {
	return `(select count(distinct moved.to_id) > 1 from artifacts moved_tree
		join graph_tuples moved on moved.from_kind = '` + BlockNodeKind + `' and moved.from_id = moved_tree.ref_key || '#' || ` + block + `
		  and moved.kind = '` + TupleInDocument + `' and moved.asserted
		where moved_tree.id = ` + artifact + ` and moved_tree.kind = 'tree')`
}

// membership is the in_document tuple a block's document_blocks row projects: the block a member
// of the document refKey names, dated joined, the row's created_at, and stated by the version's
// writer.
func membership(address, refKey string, joined time.Time, writer model.Actor) graphstore.UncausedTuple {
	return graphstore.UncausedTuple{Tuple: graphstore.Tuple{
		FromKind: BlockNodeKind, FromID: address, Kind: TupleInDocument,
		ToKind: "artifact", ToID: refKey,
		At: joined, Who: writer, Asserted: true,
	}}
}

// unwritten is nodes without each one written already holds, by kind and id, in their order: one
// write of the store takes each node once, and a caller's node beside a pass (VersionBlocks.Beside
// and PlanDecisionNodes' beside) can be one the pass writes itself - a person who both introduced
// a decision and removed an ask.
func unwritten(nodes, written []graphstore.Node) []graphstore.Node {
	if len(nodes) == 0 {
		return nil
	}
	held := make(map[[2]string]struct{}, len(written)+len(nodes))
	for _, node := range written {
		held[[2]string{node.Kind, node.ID}] = struct{}{}
	}
	kept := make([]graphstore.Node, 0, len(nodes))
	for _, node := range nodes {
		key := [2]string{node.Kind, node.ID}
		if _, already := held[key]; already {
			continue
		}
		held[key] = struct{}{}
		kept = append(kept, node)
	}
	return kept
}

// BlockInDocument is block blockID's node, live and under supertag, the supertag the typed-node
// pass gives the block (TypedFacts.supertag), and its in_document tuple, its membership of the
// document record names, both as the index pass states them (blockNode, membership): in the
// document's project or between its parties, made, dated and stated by record's writer at at, and
// caused by event. fits is false, and the two are zero, for a block whose address is past
// MaxBlockNodeBytes, which is no node of the store. It is the one statement of a block's node and
// its membership for a writer that holds its cause already: the backfill of the block rows
// written before the store (graphcensus), under the event that recorded each row's version. Such a
// block is no block a version changed (blockNode's changed).
func BlockInDocument(record IndexedVersion, blockID, supertag string, at time.Time, event int64) (node graphstore.Node, member graphstore.Tuple, fits bool) {
	if !blockIDFits(record.RefKey, blockID) {
		return graphstore.Node{}, graphstore.Tuple{}, false
	}
	address := blockNodeAddress(record.RefKey, blockID)
	node = blockNode(address, record, record.Writer, at, graphstore.StateLive, supertag, false)
	node.SourceEvent = event
	member = membership(address, record.RefKey, at, record.Writer).Tuple
	member.SourceEvent = event
	return node, member, true
}

// ReadIndexedVersion is what the version's own row and its document's row say of the pass
// (IndexedVersion), read through q in one statement, under the owner lock the pass's caller
// already holds: the document of artifactID - its ref_key, the project it belongs to or the
// session whose conversation holds it, the issue that holds it and its band (DocumentBandSQL) -
// and the authors of version `version` of the document writtenIn, which is artifactID itself but
// for a pass over an issue room, whose writer is that of the newest version it covers
// (Versions.Newest). It answers the version's whole author list, in the order its row stores it,
// which a block that version first holds records one authored_by for each of (blockAuthors), and
// the first of them as the writer, and the pass's own time (IndexedVersion.At). A version no row
// records - a caller indexing blocks of its own, as the seed does - leaves no author and the
// writer nobody, the zero actor, exactly as a node of the store written on a credential that
// names nobody carries.
func ReadIndexedVersion(ctx context.Context, q Queryer, artifactID, writtenIn string, version int) (IndexedVersion, error) {
	var record IndexedVersion
	var project, session, issue *string
	var authors []byte
	var band string
	if err := q.QueryRow(ctx, `
		select a.ref_key, a.project_key, a.session_id, a.issue_key, v.authors, `+DocumentBandSQL("a")+`, clock_timestamp()
		from artifacts a
		left join artifact_versions v on v.artifact_id = $3::uuid and v.number = $2
		where a.id = $1::uuid
	`, artifactID, version, writtenIn).Scan(&record.RefKey, &project, &session, &issue, &authors, &band, &record.At); err != nil {
		return IndexedVersion{}, fmt.Errorf("read version %d of %s for its block index: %w", version, writtenIn, err)
	}
	record.Band = band[0]
	var projectKey, conversation string
	if project != nil {
		projectKey = *project
	}
	if session != nil {
		conversation = *session
	}
	record.Project, record.Parties = graphstore.SessionScope(projectKey, conversation)
	if issue != nil {
		record.Issue = *issue
	}
	var err error
	if record.Authors, err = storedActors(authors); err != nil {
		return IndexedVersion{}, fmt.Errorf("decode the authors of version %d of %s: %w", version, writtenIn, err)
	}
	if len(record.Authors) > 0 {
		record.Writer = record.Authors[0]
	}
	return record, nil
}

// documentVersion is one version of one document: the document's artifact id and the version's
// number.
type documentVersion struct {
	document string
	number   int
}

// versionAuthors answers held with the authors of each version of wanted it does not hold yet,
// read through q in one statement: each version's authors in the order its row stores them (docs'
// actorSlice, for a settlement's), and none for a version no row records. A version the reading
// transaction has queued and not written, which no row records yet, takes the authors its queued
// row holds (Pending).
func versionAuthors(ctx context.Context, q Queryer, held map[documentVersion][]model.Actor, wanted []documentVersion) (map[documentVersion][]model.Actor, error) {
	pending := pendingOf(ctx)
	var documents []string
	var numbers []int32
	for _, version := range wanted {
		if _, known := held[version]; known {
			continue
		}
		if pending != nil {
			if authors, queued := pending.PendingAuthors(version.document, version.number); queued {
				held[version] = authors
				continue
			}
		}
		held[version] = nil
		documents, numbers = append(documents, version.document), append(numbers, int32(version.number))
	}
	if len(documents) == 0 {
		return held, nil
	}
	rows, err := q.Query(ctx, `
		select v.artifact_id::text, v.number, v.authors
		from unnest($1::uuid[], $2::int[]) as wanted (artifact_id, number)
		join artifact_versions v on v.artifact_id = wanted.artifact_id and v.number = wanted.number
	`, documents, numbers)
	if err != nil {
		return nil, fmt.Errorf("read the authors of the versions a block index pass covers: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var version documentVersion
		var authors []byte
		if err := rows.Scan(&version.document, &version.number, &authors); err != nil {
			return nil, fmt.Errorf("read the authors of a version a block index pass covers: %w", err)
		}
		if held[version], err = storedActors(authors); err != nil {
			return nil, fmt.Errorf("decode the authors of version %d of %s: %w", version.number, version.document, err)
		}
	}
	return held, rows.Err()
}

// recordsAuthors reports whether the node of a block of type typ, in a document of band band,
// records its authors by authored_by tuples and by no attribute (T7 of the issue-tree
// plan): an authored node anywhere, and every block of an issue's tree artifact (RankBandTree),
// but a decision and a reading, whose authors the typed-node pass states, a decision's by its
// decided_by (PlanDecisionNodes, TypedFacts.indexedFacts). RecordsAuthorsSQL is the same rule over
// a document_blocks row and the artifacts row it is keyed by, each named by its alias, which the
// store census reads the authors a store must hold the nodes of by (graphcensus).
func recordsAuthors(typ string, band byte) bool {
	switch typ {
	case decisionType, readingType:
		return false
	}
	return authoredType(typ) || band == RankBandTree
}

func RecordsAuthorsSQL(row, artifact string) string {
	return `(` + row + `.type in ` + AuthoredTypesSQL + ` or (` + artifact + `.issue_key is not null and ` + artifact + `.kind = 'tree'
		and ` + row + `.type not in ('` + decisionType + `', '` + readingType + `')))`
}

// blockAuthors is what a pass over document artifactID states of who wrote the blocks of
// candidates, the version's blocks whose node records its authors (recordsAuthors), by id: for
// each whose node states no current authored_by, the author nodes, each once however many blocks
// name it, and one authored_by tuple per author of the version that first holds the block, in
// that version's own order, with the aliases the server proved of each author once, all as
// graphstore.AuthorFacts states them: the tuple's far end the author's person or session node and
// its who the author's whole stored actor. The tuples are dated at the pass's own time
// (IndexedVersion.At). An author that names nobody the store can key states nothing, and two
// actors naming one node state it once. It reads through q, in the plan's read phase, before any
// of the pass's own rows is written.
//
// The version that first holds a block is the one blockFirstVersion names, the lowest
// document_block_texts records a text of it in, of its own document, where the block has such a
// row: a block written before this build, or by a pass that wrote no author - the drain of
// T6 of the issue-tree plan marks every stored document for a pass, so every such node takes its
// authors at the pass that drains its document. A block that has none yet, whose row this pass
// writes first, takes the authors of the version its text stands since (firstWritten), which is
// the row the pass records it at. A block of an issue room that has stood in more than one
// fragment has no first version Dispatch knows (MovedRoomBlockSQL), so one that moved before any
// pass recorded its authors takes none, rather than the authors of a version of another document.
//
// A block whose node states a current authored_by already takes none: a node whose first write
// carried its authors - one the conversion moved into its room, whose authors are its old
// block's - or one an earlier pass gave them. So no later writer, the conversion's own seed among
// them, is ever one of a node's authors.
func blockAuthors(ctx context.Context, q Queryer, artifactID string, record IndexedVersion, candidates []string, firstWritten func(id string) []model.Actor) ([]graphstore.Node, []graphstore.UncausedTuple, error) {
	if len(candidates) == 0 {
		return nil, nil, nil
	}
	// Whether a block of an issue room has stood in more than one of its documents is read by a
	// probe of its own in_document tuples a candidate (movedInRoom), keyed by the block's address
	// in the room's tree artifact and behind an offset 0 the planner cannot flatten, in place of
	// MovedRoomBlockSQL's join of artifacts to graph_tuples, which a plan made on the estimate of a
	// near-empty graph_tuples read once for each block, a cost that grew with the square of the
	// room (TestThePassLockTimeGrowsLinearlyWithTheRoom). A document that is no issue's tree binds
	// no room key, and none of its blocks has moved.
	var roomKey *string
	if record.Band == RankBandTree {
		roomKey = &record.RefKey
	}
	rows, err := q.Query(ctx, `
		select exists (
			select 1 from (
				`+graphstore.CurrentTupleRead(`t.to_kind, t.to_id`, `t.asserted`,
		`t.from_kind = $2 and t.from_id = $3 || '#' || block.id and t.kind = $4
				  and t.to_kind <> '`+graphstore.KindValue+`'`)+`
			) held where held.asserted),
		       first.version is not null, first.authors
		from unnest($1::text[]) with ordinality as block (id, position)
		cross join lateral (
			select count(distinct moved.to_id) > 1 as moved from (
				select t.to_id from graph_tuples t
				where t.from_kind = $2 and t.from_id = $6::text || '#' || block.id
				  and t.kind = '`+TupleInDocument+`' and t.asserted
				offset 0
			) moved
		) movedInRoom
		`+blockFirstVersionMoved("first", "$5::uuid", "block.id", "", "movedInRoom.moved")+`
		order by block.position
	`, candidates, BlockNodeKind, record.RefKey, graphstore.TupleAuthoredBy, artifactID, roomKey)
	if err != nil {
		return nil, nil, fmt.Errorf("read who wrote the first versions of the blocks of %s: %w", record.RefKey, err)
	}
	defer rows.Close()
	var nodes []graphstore.Node
	var tuples []graphstore.UncausedTuple
	// The authors whose aliases the pass states already, and every node it writes, each once:
	// a proved session's owner is a node beside the session's, and may be an author too.
	stated, made := map[blockFarEnd]struct{}{}, map[blockFarEnd]struct{}{}
	read := 0
	for ; rows.Next(); read++ {
		var recorded, recordedText bool
		var firstAuthors []byte
		if err := rows.Scan(&recorded, &recordedText, &firstAuthors); err != nil {
			return nil, nil, fmt.Errorf("read who wrote the first version of a block of %s: %w", record.RefKey, err)
		}
		if recorded || read >= len(candidates) {
			continue
		}
		id := candidates[read]
		authors := firstWritten(id)
		if recordedText {
			if authors, err = storedActors(firstAuthors); err != nil {
				return nil, nil, fmt.Errorf("decode the authors of the first version of block %s of %s: %w", id, record.RefKey, err)
			}
		}
		address := blockNodeAddress(record.RefKey, id)
		named := map[blockFarEnd]struct{}{}
		for _, author := range authors {
			authorNodes, facts, ok := graphstore.AuthorFacts(BlockNodeKind, address, author, record.At, graphstore.NoEvent)
			if !ok {
				continue
			}
			node := blockFarEnd{kind: authorNodes[0].Kind, id: authorNodes[0].ID}
			if _, twice := named[node]; twice {
				continue
			}
			named[node] = struct{}{}
			tuples = append(tuples, graphstore.UncausedTuple{Tuple: facts[0]})
			if _, again := stated[node]; again {
				continue
			}
			stated[node] = struct{}{}
			for _, alias := range facts[1:] {
				tuples = append(tuples, graphstore.UncausedTuple{Tuple: alias})
			}
			for _, authorNode := range authorNodes {
				key := blockFarEnd{kind: authorNode.Kind, id: authorNode.ID}
				if _, again := made[key]; !again {
					made[key] = struct{}{}
					nodes = append(nodes, authorNode)
				}
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("read who wrote the first versions of the blocks of %s: %w", record.RefKey, err)
	}
	if read != len(candidates) {
		return nil, nil, fmt.Errorf("read who wrote the first versions of %d blocks of %s and got %d back", len(candidates), record.RefKey, read)
	}
	return nodes, tuples, nil
}

// nodelessBlock is one block of the version whose document_blocks row names no node: its id, its
// type, which the node it takes is tagged by (TypedFacts.supertag), and the time that row was
// written, which the membership PlanBlocks states for it is dated from.
type nodelessBlock struct {
	id, typ string
	joined  time.Time
}

// blockFarEnd is one far end a block's part_of points at now.
type blockFarEnd struct{ kind, id string }

// blockPlacement is where a pass placed the blocks of a version: ids are the blocks it states
// a parent for, in document order, and beside each its index among the version's unique blocks
// (placed), the index there of the block holding it (parents, -1 for one at the document's top
// level and for one that hangs under a node that is no block of the version), and that node
// where a block hangs under one (ends, nil for every other block, and nil as a whole where there
// is none): a reading in an answered ask and a decision that settles an ask of its issue, each
// under the ask's node, and an authored node under a conversation node, a block of another
// document, or an issue that is not the document's top (placeAuthored). textParents is, beside
// each, the index among the version's unique blocks of the block whose text holds it, -1 for one
// no block of the version that is a node holds: the parent the text gives it (placeBlocks), which
// placeAuthored and an ask's node leave as it is.
//
// stored marks, beside each, a block a move of the node route placed whose part_of the pass keeps
// as the store holds it (keepStored), and unstored a block the store holds at PlacementStored that
// the pass places again, by its text, under the ask it settles or by its author (the exceptions of
// planBlockParents and placeAuthored); each is nil where it marks none. A block the store holds
// stored whose part_of is the one the pass gives it is in neither, and keeps its placement all the
// same (planBlockValues), but for a decision that settles an ask, which is unstored wherever it
// stands.
type blockPlacement struct {
	ids                          []string
	placed, parents, textParents []int
	ends                         []*blockFarEnd
	stored, unstored             []bool
}

// askPlacements is the blocks of a version that stand under an ask's node in place of the block
// their text stands in, each by its id with the id of that ask (planBlockParents): readings, each
// reading in an answered ask, and decisions, each decision that settles an ask of its issue. The
// zero value places none.
type askPlacements struct {
	readings, decisions map[string]string
}

// end sets the far end the block at index hangs under, making ends where there is none yet.
func (p *blockPlacement) end(index int, far blockFarEnd) {
	if p.ends == nil {
		p.ends = make([]*blockFarEnd, len(p.ids))
	}
	p.ends[index] = &far
}

// detachTextParents gives textParents a slice of its own where it still shares parents', so a
// block placed elsewhere than its text keeps the text parent its text gives it.
func (p *blockPlacement) detachTextParents() {
	if len(p.textParents) > 0 && &p.textParents[0] == &p.parents[0] {
		p.textParents = slices.Clone(p.parents)
	}
}

// keepStored marks the block at index as one whose stored part_of the pass keeps.
func (p *blockPlacement) keepStored(index int) {
	if p.stored == nil {
		p.stored = make([]bool, len(p.ids))
	}
	p.stored[index] = true
}

// unstore marks the block at index as one the store holds stored that the pass places again.
func (p *blockPlacement) unstore(index int) {
	if p.unstored == nil {
		p.unstored = make([]bool, len(p.ids))
	}
	p.unstored[index] = true
}

// keepsStored says the block at index keeps the placement PlacementStored once the pass has run:
// the pass kept its stored part_of (keepStored), or the store holds it stored, at the part_of the
// text gives it, and the pass did not place it again (unstore). held is the placement the store
// holds of it.
func (p blockPlacement) keepsStored(index int, held string) bool {
	if p.stored != nil && p.stored[index] {
		return true
	}
	return held == PlacementStored && (p.unstored == nil || !p.unstored[index])
}

// removedParent says far, the part_of a stored block holds, is a block of the indexed document
// (refKey) that the version no longer holds: the second exception, under which the pass places
// the block again rather than leave it under a gone parent. held is each block id of the version.
func removedParent(far blockFarEnd, refKey string, held map[string]struct{}) bool {
	if far.kind != BlockNodeKind {
		return false
	}
	id, own := strings.CutPrefix(far.id, refKey+"#")
	if !own {
		return false
	}
	_, holds := held[id]
	return !holds
}

// parentProbe is the blocks of a placement heldBlockParentDifferences reads, as the arrays it
// binds: each block's id, the block holding it (parentIDs, the projection), and the kind and the
// id of the node it hangs under in place of either (blockPlacement.ends). positions is each
// probed block's index in the placement, nil where every block of it is probed.
type parentProbe struct {
	ids                       []string
	parents, endKinds, endIDs []*string
	positions                 []int
}

// probe is the parentProbe of every block of the placement but those skipped: the kept, which
// state nothing, and those an earlier indexing of the transaction placed, which the store does not
// hold yet (planBlockParents).
func (p blockPlacement) probe(parentIDs []*string, skipped []bool) parentProbe {
	probe := parentProbe{ids: p.ids, parents: parentIDs}
	if slices.Contains(skipped, true) {
		probe.ids, probe.parents = nil, nil
		for index, skip := range skipped {
			if !skip {
				probe.ids, probe.parents = append(probe.ids, p.ids[index]), append(probe.parents, parentIDs[index])
				probe.positions = append(probe.positions, index)
			}
		}
	}
	if p.ends == nil {
		return probe
	}
	probe.endKinds, probe.endIDs = make([]*string, len(probe.ids)), make([]*string, len(probe.ids))
	for position := range probe.ids {
		if end := p.ends[probe.position(position)]; end != nil {
			probe.endKinds[position], probe.endIDs[position] = &end.kind, &end.id
		}
	}
	return probe
}

// position is the placement's index of the block probed at position.
func (p parentProbe) position(position int) int {
	if p.positions == nil {
		return position
	}
	return p.positions[position]
}

// planBlockParents plans the parent of every block of the version and its projection into
// document_blocks (queueBlockParents). It answers where it placed each block, which the pass's
// values are stated against (planBlockValues), the tuples it states, which PlanBlocks appends to
// the pass's write as it appends the reference pass's, and where each block sits once the
// indexing is written, for a later indexing of the document in the same transaction (Held).
//
// It reads the parent each block holds in the store, over every block of the document and not
// the changed ones alone, so a block another block now holds, whose own text no version
// changed, gets its new parent here. It states a tuple only where the render disagrees with
// what the store asserts: one retraction for the parent that changed, one assertion for the new
// one, both dated at the pass's own time (IndexedVersion.At). A re-index of a document nothing
// moved states nothing, which is what keeps a second pass from asserting a parent twice, since
// no code may update or delete a tuple row. A block an earlier indexing of this transaction
// placed (BlockIndex.placed) is compared with where that indexing put it, which the store holds
// only once the write phase has run.
//
// A block no block holds hangs under the document's top (IndexedVersion.top): the issue for a
// block of an issue's spec, Verification or plan document, and the document's own node for any
// other. A block an earlier build placed under its document's node is moved under the issue by
// the first pass that reaches it, as any other block that moved is. An authored node is the one
// block the render does not place: its author does (placeAuthored, D4 of the issue-tree
// plan), and one whose part_of stands is no part of the comparison at all.
//
// In an issue room each fragment's top-level blocks hang under their own document's top
// (IndexedVersion.topOf, over fragments, the documents whose fragments hold the blocks): an
// upload's under the upload's node, as an upload that is its own room hangs them.
//
// The comparison is the read's own (heldBlockParentDifferences): the parent the render gives
// travels with the probe, so a document nothing moved reads back no row at all rather than one
// row per block for Go to compare. Both statements name a block by its id and build its node
// address from the document's ref_key, as the insert of document_blocks does, so a pass over
// thousands of blocks holds no address for the thousands it states nothing about. A block at
// the top projects a null parent_block_id, whichever node its part_of names, and so does an
// authored node under any node that is no block of the indexed artifact.
//
// asks is the blocks of the version that stand under an ask's node in place of the block their
// text stands in, each by its id with that ask's id (DecisionFacts, TypedFacts.asks): each reading
// in an answered ask, since the decision a reading there reads is the person's answer the node
// holds, and each decision that settles an ask of its issue, "a decision under the ask it
// answers" (R2 of the issue-tree spec). document_blocks.parent_block_id keeps the id of the block
// whose text holds each - for a reading the ask block holding it, which is that tuple's
// projection - and so does its text_parent (planBlockValues), so a decision stays where its text
// stands in its document. A decision that settles an ask stands under it whatever its placement
// (R7's third exception): a part_of a move of the node route stored is placed again, under the
// ask (unstore), and the decision takes back the placement PlacementText. only, where it is not
// nil, is the blocks the pass states a parent for, the typed-node pass's own above the deferral
// bound (PlanDecisionNodes); every block is placed in the version all the same, so the parent each
// of them takes is the one a pass over the whole version gives it. authored is what the version
// writes of where its authored nodes go (authoredSourcesOf), the zero value for a pass that reads
// none. Every read is q's, in the plan's read phase.
func (b *BlockIndexing) planBlockParents(
	ctx context.Context,
	q Queryer,
	indexed BlockIndex,
	record IndexedVersion,
	nodes []model.ArtifactBlock,
	fits []bool,
	fragments map[string]fragmentDocument,
	asks askPlacements,
	only map[string]struct{},
	authored authoredSources,
) (blockPlacement, []graphstore.UncausedTuple, map[string]blockFarEnd, error) {
	standing := maps.Clone(indexed.placed)
	if len(nodes) == 0 {
		return blockPlacement{}, nil, standing, nil
	}
	refKey := record.RefKey
	placed, parents := placeBlocks(nodes, fits)
	if only != nil {
		kept := 0
		for position, block := range placed {
			if _, own := only[nodes[block].ID]; own {
				placed[kept], parents[kept] = block, parents[position]
				kept++
			}
		}
		placed, parents = placed[:kept], parents[:kept]
	}
	if len(placed) == 0 {
		return blockPlacement{}, nil, standing, nil
	}
	if standing == nil {
		standing = make(map[string]blockFarEnd, len(placed))
	}
	placement := blockPlacement{ids: make([]string, len(placed)), placed: placed, parents: parents, textParents: parents}
	// The parent the text gives each block is its text parent (planBlockValues), whatever
	// placeAuthored then makes of an authored node's: a copy of the parents is kept only where the
	// placement holds an authored node, the one block placeAuthored moves.
	for _, block := range placed {
		if authoredType(nodes[block].Type) {
			placement.detachTextParents()
			break
		}
	}
	blockIDs := placement.ids
	parentIDs := make([]*string, len(placed))
	for index, block := range placed {
		blockIDs[index] = nodes[block].ID
		if parents[index] >= 0 {
			parentIDs[index] = &nodes[parents[index]].ID
		}
		if ask, held := asks.readings[blockIDs[index]]; held {
			placement.end(index, blockFarEnd{kind: graphstore.KindAsk, id: ask})
		}
		if ask, settles := asks.decisions[blockIDs[index]]; settles {
			placement.end(index, blockFarEnd{kind: graphstore.KindAsk, id: ask})
			placement.unstore(index)
		}
	}
	kept, err := placeAuthored(ctx, q, b.artifactID, indexed, record, nodes, &placement, parentIDs, authored, b.outside)
	if err != nil {
		return blockPlacement{}, nil, nil, fmt.Errorf("place the authored nodes of %s: %w", b.artifactID, err)
	}
	topKind, topID := record.top()
	for index, block := range placed {
		if parents[index] >= 0 || authoredType(nodes[block].Type) || placement.ends != nil && placement.ends[index] != nil {
			continue
		}
		if kind, id := record.topOf(nodes[block], fragments); kind != topKind || id != topID {
			placement.end(index, blockFarEnd{kind: kind, id: id})
		}
	}
	// Where each block of the placement hangs once the pass is written, by its place among them:
	// the node it hangs under in place of a block, else the block holding it, else the top.
	wanted := make([]blockFarEnd, len(placed))
	for index := range placed {
		wanted[index] = blockFarEnd{kind: topKind, id: topID}
		switch parent := parentIDs[index]; {
		case placement.ends != nil && placement.ends[index] != nil:
			wanted[index] = *placement.ends[index]
		case parent != nil:
			wanted[index] = blockFarEnd{kind: BlockNodeKind, id: blockNodeAddress(refKey, *parent)}
		}
	}
	// Each placed block's difference, by its place among them: a kept node states none, a block an
	// earlier indexing of this transaction placed is compared with where that indexing put it, and
	// the rest are read from the store in one probe.
	//
	// A block the store holds at PlacementStored keeps the part_of it holds (keepStored): the move
	// of the node route that placed it decides its parent, and its text does not. There are two
	// exceptions here. A stored part_of to a block of the document this version no longer holds
	// (removedParent) places the block by its text again (unstore), so no stored block stands
	// under a gone parent; and a decision that settles an ask of its issue (asks.decisions) stands
	// under that ask whatever its placement, so its stored part_of is never kept and the pass
	// unstores it above. An authored node's exceptions are placeAuthored's. A block a move of this
	// same transaction placed is kept where the move put it (BlockIndex.storedMove), which the store
	// holds only once the write phase has run; no such move is of a decision that settles an ask,
	// which the route refuses to move (api's NODE_NOT_MOVABLE).
	var versionIDs map[string]struct{}
	removed := func(far blockFarEnd) bool {
		if versionIDs == nil {
			versionIDs = versionBlockIDs(nodes)
		}
		return removedParent(far, refKey, versionIDs)
	}
	settles := func(position int) bool {
		_, settling := asks.decisions[blockIDs[position]]
		return settling
	}
	places := authoredPlaces{document: record, nodes: nodes, placement: &placement, parentIDs: parentIDs, outside: b.outside}
	keep := func(position int, far blockFarEnd) {
		placement.detachTextParents()
		places.set(position, far)
		placement.keepStored(position)
		wanted[position] = far
	}
	skipped := make([]bool, len(placed))
	differences := make([]*blockParentDifference, len(placed))
	// A prior pass of the same transaction that placed a block without reading its values
	// (PlanDecisionNodes) records indexed.placed[id] and leaves indexed.values[id] absent. Where
	// that cached far end disagrees with this pass's wanted parent, the block's placement is read
	// from the store before deciding whether PlacementStored keeps the cached parent.
	var unvalued []int
	var unvaluedIDs []string
	applyCached := func(position int, far blockFarEnd, placementValue string) {
		if placementValue == PlacementStored {
			if !removed(far) && !settles(position) {
				keep(position, far)
				return
			}
			placement.unstore(position)
		}
		differences[position] = &blockParentDifference{block: position, retract: []blockFarEnd{far}}
	}
	for position, id := range blockIDs {
		if kept != nil && kept[position] {
			skipped[position] = true
			continue
		}
		if move, moved := indexed.storedMove(blockNodeAddress(refKey, id)); moved {
			skipped[position] = true
			keep(position, blockFarEnd{kind: move.ParentKind, id: move.ParentID})
			continue
		}
		far, known := indexed.placed[id]
		if !known {
			continue
		}
		skipped[position] = true
		if far == wanted[position] {
			continue
		}
		if value, valued := indexed.values[id]; valued {
			applyCached(position, far, value.placement)
			continue
		}
		unvalued, unvaluedIDs = append(unvalued, position), append(unvaluedIDs, id)
	}
	if len(unvalued) > 0 {
		heldPlacements, err := heldBlockPlacements(ctx, q, refKey, unvaluedIDs)
		if err != nil {
			return blockPlacement{}, nil, nil, fmt.Errorf("read the placements the blocks of %s hold: %w", b.artifactID, err)
		}
		for index, position := range unvalued {
			applyCached(position, indexed.placed[blockIDs[position]], heldPlacements[index])
		}
	}
	probe := placement.probe(parentIDs, skipped)
	read, err := heldBlockParentDifferences(ctx, q, refKey, topKind, topID, probe)
	if err != nil {
		return blockPlacement{}, nil, nil, fmt.Errorf("read the parents the blocks of %s hold: %w", b.artifactID, err)
	}
	for _, difference := range read {
		difference.block = probe.position(difference.block)
		if difference.placement == PlacementStored {
			if len(difference.retract) == 1 && !difference.holds && !removed(difference.retract[0]) && !settles(difference.block) {
				keep(difference.block, difference.retract[0])
				continue
			}
			placement.unstore(difference.block)
		}
		differences[difference.block] = &difference
	}
	var tuples []graphstore.UncausedTuple
	for position, difference := range differences {
		standing[blockIDs[position]] = wanted[position]
		if difference == nil {
			continue
		}
		address := blockNodeAddress(refKey, blockIDs[position])
		for _, far := range difference.retract {
			tuples = append(tuples, parentTuple(address, far.kind, far.id, record.At, record.Writer, false))
		}
		if difference.holds {
			continue
		}
		tuples = append(tuples, parentTuple(address, wanted[position].kind, wanted[position].id, record.At, record.Writer, true))
	}
	b.placedIDs, b.placedParents = blockIDs, parentIDs
	return placement, tuples, standing, nil
}

// queueBlockParents queues the projection of the parent of every placed block whose row the
// indexing's insert does not write (standingPlaced) into document_blocks: a block that only moved
// keeps its changed_version and changed_at, so its row is no row of that insert, and a row the
// insert writes carries its parent and its node columns already (queueRows, rowParents). The
// statement carries the node columns beside the parent, so a row the insert did not touch learns
// the address of its node here, and it updates only a row whose parent or node columns differ.
func (b BlockIndexing) queueBlockParents(batch *store.WriteBatch) {
	ids, parents := b.standingPlaced()
	if len(ids) == 0 {
		return
	}
	batch.Queue(fmt.Sprintf("project the parents of the blocks of %s", b.artifactID), `
		update document_blocks d
		set node_kind = $3, node_address = $4 || '#' || p.block_id, parent_block_id = p.parent
		from unnest($2::text[], $5::text[]) as p (block_id, parent)
		where d.artifact_id = $1::uuid and d.block_id = p.block_id
		  and (d.parent_block_id is distinct from p.parent
		       or d.node_kind is distinct from $3
		       or d.node_address is distinct from $4 || '#' || p.block_id)
	`, b.artifactID, ids, BlockNodeKind, b.refKey, parents)
}

// rowParents is, beside each block whose row the indexing's insert writes (ids), the block the
// placement holds it in (placedIDs, placedParents): nil for a block the document holds at its top
// level, and for one the pass places nowhere, whose address is past the store's bound. Both lists
// are in document order, so one walk forward pairs them.
func (b BlockIndexing) rowParents() []*string {
	parents := make([]*string, len(b.ids))
	next := 0
	for position, id := range b.ids {
		if found := slices.Index(b.placedIDs[next:], id); found >= 0 {
			parents[position], next = b.placedParents[next+found], next+found+1
		}
	}
	return parents
}

// standingPlaced is each placed block whose row the indexing's insert does not write, with the
// block the placement holds it in: the placement less ids, both in document order, so one walk
// forward parts them. An indexing whose insert writes no row answers the placement itself.
func (b BlockIndexing) standingPlaced() ([]string, []*string) {
	if len(b.ids) == 0 {
		return b.placedIDs, b.placedParents
	}
	ids, parents := make([]string, 0, len(b.placedIDs)), make([]*string, 0, len(b.placedIDs))
	next := 0
	for _, id := range b.ids {
		found := slices.Index(b.placedIDs[next:], id)
		if found < 0 {
			continue
		}
		ids, parents = append(ids, b.placedIDs[next:next+found]...), append(parents, b.placedParents[next:next+found]...)
		next += found + 1
	}
	return append(ids, b.placedIDs[next:]...), append(parents, b.placedParents[next:]...)
}

// The block types whose node is an authored node (D4 of the issue-tree plan): its parent
// is its author's to decide, which its text cannot, since a check sits after the spec's words and
// not inside a requirement. AuthoredTypesSQL is the same list as SQL, which the store census reads
// an authored node's row by (graphcensus).
const (
	criterionType   = "criterion"
	requirementType = "requirement"
	checkType       = graphstore.BlockTypeCheck
	planTaskType    = "plan_task"

	AuthoredTypesSQL = `('` + criterionType + `', '` + requirementType + `', '` + checkType + `', '` + planTaskType + `')`
)

// servesKind is the pointer an authored node names what it serves by: a relation of its own,
// many to many, whose first target of the node's own issue is the node's first parent and no
// more (R4 of the issue-tree spec, `r-serves-coverage`).
const servesKind = "serves"

// authoredType reports whether a block of type typ is an authored node.
func authoredType(typ string) bool {
	switch typ {
	case criterionType, requirementType, checkType, planTaskType:
		return true
	}
	return false
}

// authoredSources is what a version writes of where each of its authored nodes goes, by block id
// (placeAuthored): the target its parent attribute holds (pmdoc.ParentAttribute, at most one),
// the targets its serves pointer holds in written order, and for a requirement the references its
// From: line holds in the order the line writes them (fromLine).
type authoredSources struct {
	parent       map[string]text.Ref
	serves, from map[string][]text.Ref
}

// authoredSourcesOf reads authoredSources off a version: its unique blocks nodes, the mentions
// located in its markdown, in document order, and its pointers, each taken only off the node it
// is written on, as pointerPairs takes one: the first holder of its block id, of its own type.
func authoredSourcesOf(markdown string, nodes []model.ArtifactBlock, located []text.Located, pointers []Pointer) authoredSources {
	var sources authoredSources
	var types map[string]string
	for _, block := range nodes {
		if !authoredType(block.Type) {
			continue
		}
		if types == nil {
			types = map[string]string{}
		}
		types[block.ID] = block.Type
		if block.Type != requirementType {
			continue
		}
		start, end, written := fromLine(markdown[block.From:block.To])
		if !written {
			continue
		}
		start, end = block.From+start, block.From+end
		first := sort.Search(len(located), func(index int) bool { return located[index].Offset >= start })
		for _, mention := range located[first:] {
			if mention.Offset >= end {
				break
			}
			if sources.from == nil {
				sources.from = map[string][]text.Ref{}
			}
			sources.from[block.ID] = append(sources.from[block.ID], mention.Ref)
		}
	}
	for _, pointer := range pointers {
		if typ, ok := types[pointer.Block]; !ok || typ != pointer.BlockType {
			continue
		}
		switch pointer.Kind {
		case pmdoc.ParentAttribute:
			if sources.parent == nil {
				sources.parent = map[string]text.Ref{}
			}
			if _, held := sources.parent[pointer.Block]; !held {
				sources.parent[pointer.Block] = pointer.Target
			}
		case servesKind:
			if sources.serves == nil {
				sources.serves = map[string][]text.Ref{}
			}
			sources.serves[pointer.Block] = append(sources.serves[pointer.Block], pointer.Target)
		}
	}
	return sources
}

// fromLine is the span of a requirement's From: line in its text body, the line that names where
// the requirement came from (R3 and R22 of the issue-tree spec): from the last "From:"
// that begins a word outside a code span to the end of its line, so words that quote the label in
// code - "reads its `From:` line" - name no source, and the line the requirement ends on does. A
// code span is markdown's: a run of backticks opens one and the next run of as many closes it,
// while a run no such run closes, and a backtick a backslash escapes, is text (CommonMark's rule,
// which a fenced code block's two fences meet too). written is false for a text that holds none.
func fromLine(body string) (start, end int, written bool) {
	const label = "From:"
	start = -1
	for offset := 0; offset < len(body); {
		switch {
		case body[offset] == '\\' && offset+1 < len(body) && body[offset+1] == '`':
			offset += 2
		case body[offset] == '`':
			offset = codeSpanEnd(body, offset)
		case strings.HasPrefix(body[offset:], label) && (offset == 0 || !wordByte(body[offset-1])):
			start = offset
			offset += len(label)
		default:
			offset++
		}
	}
	if start < 0 {
		return 0, 0, false
	}
	if line := strings.IndexByte(body[start:], '\n'); line >= 0 {
		return start, start + line, true
	}
	return start, len(body), true
}

// codeSpanEnd is the offset past the code span the run of backticks at open opens in body: past
// the next run of exactly as many backticks, or, where no such run closes it, past the run
// itself, which is then text.
func codeSpanEnd(body string, open int) int {
	run := backtickRun(body, open)
	for at := open + run; at < len(body); {
		if body[at] != '`' {
			at++
			continue
		}
		closing := backtickRun(body, at)
		if closing == run {
			return at + closing
		}
		at += closing
	}
	return open + run
}

// backtickRun is how many backticks run in body from at.
func backtickRun(body string, at int) int {
	run := 0
	for at+run < len(body) && body[at+run] == '`' {
		run++
	}
	return run
}

// wordByte reports whether b is an ASCII letter, digit or underscore, which a label beginning a
// word does not follow.
func wordByte(b byte) bool {
	return b == '_' || b >= '0' && b <= '9' || b >= 'A' && b <= 'Z' || b >= 'a' && b <= 'z'
}

// heldAuthored is what the store holds now of one authored node (placeAuthored): its current
// placement, "" where it holds none; the far end of its current part_of, the zero far end where
// it states none; and the far end of its current parent value, the target its parent attribute
// held in the version the store last stated it in, which planBlockRefs keeps (the zero far
// end where it held none).
type heldAuthored struct {
	placement       string
	parent, pointer blockFarEnd
}

// heldAuthoredBlocks reads, in one statement, heldAuthored of each block of ids, in ids' order,
// each block named by its id with its address built from refKey as every statement of the pass
// builds it: its placement as heldBlockValues reads it, through graph_tuples_values, and its
// part_of and its parent value as heldBlockParentDifferences reads a relation, the newest tuple
// per far end kept while it asserts (graphstore.CurrentTupleRead), through graph_tuples_current.
func heldAuthoredBlocks(ctx context.Context, q Queryer, refKey string, ids []string) ([]heldAuthored, error) {
	current := func(kind string) string {
		return `select held.to_kind, held.to_id from (
				` + graphstore.CurrentTupleRead(`t.to_kind, t.to_id`, `t.to_kind, t.to_id, t.asserted, t.seq`,
			`t.from_kind = $2 and t.from_id = $3 || '#' || block.id and t.kind = `+kind+`
				  and t.to_kind <> '`+graphstore.KindValue+`'`) + `
			) held
			where held.asserted
			order by held.seq desc limit 1`
	}
	rows, err := q.Query(ctx, `
		select coalesce(placement.held, ''), coalesce(parent.to_kind, ''), coalesce(parent.to_id, ''),
		       coalesce(pointer.to_kind, ''), coalesce(pointer.to_id, '')
		from unnest($1::text[]) with ordinality as block (id, position)
		left join lateral (
			select case when t.asserted then t.to_id end as held from graph_tuples t
			where t.from_kind = $2 and t.from_id = $3 || '#' || block.id and t.kind = $4
			  and t.to_kind = '`+graphstore.KindValue+`'
			order by t.seq desc limit 1
		) placement on true
		left join lateral (`+current("$5")+`) parent on true
		left join lateral (`+current("$6")+`) pointer on true
		order by block.position
	`, ids, BlockNodeKind, refKey, AttributePlacement, graphstore.TuplePartOf, pmdoc.ParentAttribute)
	if err != nil {
		return nil, fmt.Errorf("read where the authored nodes of %s stand: %w", refKey, err)
	}
	defer rows.Close()
	held := make([]heldAuthored, 0, len(ids))
	for rows.Next() {
		var node heldAuthored
		if err := rows.Scan(&node.placement, &node.parent.kind, &node.parent.id, &node.pointer.kind, &node.pointer.id); err != nil {
			return nil, fmt.Errorf("read where an authored node of %s stands: %w", refKey, err)
		}
		held = append(held, node)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read where the authored nodes of %s stand: %w", refKey, err)
	}
	if len(held) != len(ids) {
		return nil, fmt.Errorf("read where %d authored nodes of %s stand and got %d back", len(ids), refKey, len(held))
	}
	return held, nil
}

// writtenParent is the far end the parent attribute of block id names in sources, as
// planBlockRefs states its tuple: a block resolved through farEnds (authoredFarEnds), as that
// write resolves a pointer's far end, and the zero far end where it names none or names a target
// the store cannot key, of which no tuple is stated either.
func writtenParent(sources authoredSources, farEnds map[string]string, id string) blockFarEnd {
	ref, written := sources.parent[id]
	if !written {
		return blockFarEnd{}
	}
	target, ok := TargetOf(ref)
	if !ok {
		return blockFarEnd{}
	}
	far := resolvedFarEnd(target, farEnds)
	if !graphstore.AddressFits(far.id) {
		return blockFarEnd{}
	}
	return far
}

// resolvedFarEnd is target as a far end, a block's address taken as farEnds resolves it
// (ResolveFarEnds) and every other target as it is.
func resolvedFarEnd(target Target, farEnds map[string]string) blockFarEnd {
	if node, moved := farEnds[target.ID]; moved && target.Kind == BlockNodeKind {
		return blockFarEnd{kind: target.Kind, id: node}
	}
	return blockFarEnd{kind: target.Kind, id: target.ID}
}

// authoredFarEnds resolves, in one statement, each block far end the parent attribute and the
// serves pointer of the authored nodes ids name in sources, as planBlockRefs resolves the far ends
// of the same pointers when it writes their tuples (resolvePassFarEnds, over the blocks written,
// the addresses of the blocks of document artifactID this pass places): so a node is placed under
// the node its pointer's tuple reaches - a block of another fragment of the issue room the pass
// indexes among them, whose node the pass itself writes - and its parent attribute compared with
// the parent value the store holds in the same terms (placeAuthored).
func authoredFarEnds(ctx context.Context, q Queryer, artifactID string, written, ids []string, sources authoredSources) (map[string]string, error) {
	var addresses []string
	add := func(ref text.Ref) {
		if target, ok := TargetOf(ref); ok && target.Kind == BlockNodeKind {
			addresses = append(addresses, target.ID)
		}
	}
	for _, id := range ids {
		if ref, named := sources.parent[id]; named {
			add(ref)
		}
		for _, ref := range sources.serves[id] {
			add(ref)
		}
	}
	return resolvePassFarEnds(ctx, q, artifactID, addresses, written)
}

// placeAuthored places the authored nodes of placement (D4 of the issue-tree plan),
// which placeBlocks placed by their text like any other block. An authored node takes its first
// parent at the pass that first places it as one - its placement is not PlacementAuthored yet,
// or it states no part_of - by one precedence (authoredPlaces.candidates): its parent attribute's
// target; else the first target of its serves pointer, in written order, that is a node of its
// own issue (decision 5); else, for a requirement, the first reference of its From: line that is
// a conversation node of its own issue (R3 and R22 at spec version 83); else its issue. After that
// its part_of moves only where its parent attribute names another target than the parent value
// the store holds of the version it last stated, and never because a serves target, a From: line
// or the rendering moved: such a node is kept - it is in no comparison, so nothing is stated of
// its place - and it stands where its part_of puts it. A store another release indexed the
// document into since this one last stated it (BlockIndex's stale stand) changes none of that:
// the release before writes no tuple, so the placement, the part_of and the parent value the
// store holds are this release's own, and the projection a kept node's part_of gives is written
// again by the pass (planBlockParents, queueBlockParents), over whatever row that release wrote.
// It reads the store through q, in the plan's read phase: a node an earlier indexing of the same
// transaction placed reads as the store holds it, not as that indexing queued it.
//
// It answers which blocks of placement it kept, and writes into placement where each authored
// node stands - under a block of the version, or under a node that is none (blockPlacement.end)
// - and into parentIDs that place's projection: the parent's block id where the parent is a
// block of the same indexed artifact, and null where it is the issue, a conversation node or a
// block of another artifact.
//
// No parent that would close a part_of cycle is taken, whichever rule names it, since a cycle
// leaves no walk of the tree an end. Once every node the pass places stands at its first
// candidate, the pass walks each one's chain up (authoredPlaces.cycles): through this pass's
// placement for a block of the version, and up the store's chain (PartOfChains) from any other
// block, back into the placement at the first block of the version that chain reaches. A node
// whose walk returns to it falls through to its next candidate, as for a target that names
// nothing, until its walk ends; its last candidate, its issue or its document's top, ends every
// walk. A cycle the moves made runs through the node whose move made it, so walking each node
// once, in document order, leaves none. Two passes of two documents placing each other's blocks
// at once cannot each miss the other's part_of: a serves target is a node of the node's own
// issue, and a parent of another issue is one of an issue above it, which can name no block of
// an issue below it, so a cycle runs through documents of one issue alone, whose passes each hold
// that issue's owner lock (PlanBlocks' caller), so the second waits for the first to commit, and
// under READ COMMITTED each of its statements, the chain read among them, reads what it committed.
//
// What the store holds of an authored node an earlier indexing of this transaction placed is read
// from that indexing's Held (indexed) rather than from the store, which holds what it queued only
// once the write phase has run: its placement value (BlockIndex.values), its part_of
// (BlockIndex.placed) and its parent value, the target of the parent attribute's link its pairs
// hold (BlockIndex.links). So a second indexing of the document in one transaction keeps a node
// where the first placed it, as a later transaction would.
//
// An authored node a move of the node route placed holds PlacementStored, and it is kept where
// the move put it as an authored node is kept, while its parent attribute names the target the
// parent value the store holds names - the move rewrites the attribute to match its new parent, or
// takes it out where it can name none - and while its part_of names no block of the document this
// version no longer holds (removedParent). An edit whose text names another target places it again
// by the precedence above, under PlacementAuthored, so the author's newest write wins; a removed
// parent places it again the same way. A node a move of this same transaction placed
// (BlockIndex.storedMove) is kept where the move put it, since the move wrote the attribute in
// that transaction to match.
func placeAuthored(ctx context.Context, q Queryer, artifactID string, indexed BlockIndex, document IndexedVersion, nodes []model.ArtifactBlock, placement *blockPlacement, parentIDs []*string, sources authoredSources, outside map[string]struct{}) ([]bool, error) {
	var authored []int
	for index, block := range placement.placed {
		if authoredType(nodes[block].Type) {
			authored = append(authored, index)
		}
	}
	if len(authored) == 0 {
		return nil, nil
	}
	ids := make([]string, len(authored))
	for position, index := range authored {
		ids[position] = placement.ids[index]
	}
	held := make([]heldAuthored, len(ids))
	var unread []string
	var unreadAt []int
	// The parent attribute each block's links hold, the first one where a block holds more, read
	// once for every held block rather than by a walk of the links for each.
	var pointers map[string]blockFarEnd
	for position, id := range ids {
		far, placed := indexed.placed[id]
		value, valued := indexed.values[id]
		if !placed || !valued {
			unread, unreadAt = append(unread, id), append(unreadAt, position)
			continue
		}
		if pointers == nil {
			pointers = make(map[string]blockFarEnd)
			for _, link := range indexed.links {
				if _, again := pointers[link.blockID]; link.kind == pmdoc.ParentAttribute && !again {
					pointers[link.blockID] = blockFarEnd{kind: link.target.Kind, id: link.target.ID}
				}
			}
		}
		held[position] = heldAuthored{placement: value.placement, parent: far, pointer: pointers[id]}
	}
	if len(unread) > 0 {
		read, err := heldAuthoredBlocks(ctx, q, document.RefKey, unread)
		if err != nil {
			return nil, err
		}
		for position, at := range unreadAt {
			held[at] = read[position]
		}
	}
	written := make([]string, len(placement.ids))
	for position, id := range placement.ids {
		written[position] = blockNodeAddress(document.RefKey, id)
	}
	farEnds, err := authoredFarEnds(ctx, q, artifactID, written, ids, sources)
	if err != nil {
		return nil, err
	}
	places := authoredPlaces{document: document, nodes: nodes, placement: placement, parentIDs: parentIDs, outside: outside}
	kept := make([]bool, len(placement.ids))
	var placing []int
	// The blocks no version of this pass holds that kept nodes hang under, whose chains the walk
	// climbs on through as the store holds them.
	var keptEnds []string
	var versionIDs map[string]struct{}
	keep := func(index int, far blockFarEnd) {
		kept[index] = true
		places.set(index, far)
		if _, inVersion := places.ownBlock(far); far.kind == BlockNodeKind && inVersion < 0 {
			keptEnds = append(keptEnds, far.id)
		}
	}
	for position, index := range authored {
		stored := held[position]
		if move, moved := indexed.storedMove(blockNodeAddress(document.RefKey, ids[position])); moved {
			keep(index, blockFarEnd{kind: move.ParentKind, id: move.ParentID})
			placement.keepStored(index)
			continue
		}
		if stored.placement == PlacementStored && stored.parent.kind != "" {
			if versionIDs == nil {
				versionIDs = versionBlockIDs(nodes)
			}
			if !removedParent(stored.parent, document.RefKey, versionIDs) &&
				stored.pointer == writtenParent(sources, farEnds, ids[position]) {
				keep(index, stored.parent)
				placement.keepStored(index)
				continue
			}
			placement.unstore(index)
			placing = append(placing, index)
			continue
		}
		if stored.placement == PlacementAuthored && stored.parent.kind != "" &&
			stored.pointer == writtenParent(sources, farEnds, ids[position]) {
			keep(index, stored.parent)
			continue
		}
		placing = append(placing, index)
	}
	if len(placing) == 0 {
		return kept, nil
	}
	resolved, err := resolveAuthored(ctx, q, document, placement, placing, sources, keptEnds, farEnds)
	if err != nil {
		return nil, err
	}
	candidates := make([][]blockFarEnd, len(placing))
	for position, index := range placing {
		candidates[position] = places.candidates(index, sources, resolved)
		places.set(index, candidates[position][0])
	}
	for position, index := range placing {
		for taken := 0; taken+1 < len(candidates[position]) && places.cycles(index, resolved.chains); {
			taken++
			places.set(index, candidates[position][taken])
		}
	}
	return kept, nil
}

// versionBlockIDs is the set of the ids of nodes, the blocks of the version a pass places, which a
// placement reads a stored parent's block against (removedParent), built where it reads one.
func versionBlockIDs(nodes []model.ArtifactBlock) map[string]struct{} {
	ids := make(map[string]struct{}, len(nodes))
	for _, node := range nodes {
		ids[node.ID] = struct{}{}
	}
	return ids
}

// authoredResolution is what the store says of the targets the authored nodes a pass places name
// (resolveAuthored): each block far end, by its address, resolved to another node than itself
// (authoredFarEnds), which every read below takes in the address's place; each ask, comment or
// message, or block of one, by node id, with its item's own issue and whether it is a
// conversation node at the pass's time (conversationTargets); each block of another document that
// is a node of the store, by node id, with its document's issue, "" for a document no issue holds;
// the chain of far ends the part_of of each such block, and of each block no version of the pass
// holds that a kept node hangs under, climbs through as the store holds it now, nearest first, at
// most authoredChainDepth long (PartOfChains, which the cycle walk climbs,
// authoredPlaces.cycles); and the issues above the indexed document's own, read only where a
// parent attribute names something of another issue.
type authoredResolution struct {
	farEnds   map[string]string
	items     map[string]pmdoc.BlockTarget
	blocks    map[string]string
	chains    map[string][]Target
	ancestors []string
}

// authoredChainDepth bounds the climb from a block no version of a pass holds (PartOfChains): a
// chain longer than that is read as reaching no node of the pass. A tree of an issue is a few
// levels deep - a conversation node, a criterion, a requirement, a check - so a real chain is far
// shorter.
const authoredChainDepth = 64

// resolveAuthored reads authoredResolution for the authored nodes of placement at placing, and
// the chains of keptEnds besides, in at most four statements however many nodes name however
// many targets, beside the one that resolved farEnds.
func resolveAuthored(ctx context.Context, q Queryer, document IndexedVersion, placement *blockPlacement, placing []int, sources authoredSources, keptEnds []string, farEnds map[string]string) (authoredResolution, error) {
	var items []text.Ref
	var blocks []string
	own := document.RefKey + "#"
	add := func(ref text.Ref, item, block bool) {
		switch {
		case pmdoc.ItemKindOf(ref) != "":
			if item {
				items = append(items, ref)
			}
		case block && ref.Kind == BlockNodeKind:
			target, _ := TargetOf(ref)
			if id := resolvedFarEnd(target, farEnds).id; !strings.HasPrefix(id, own) && !slices.Contains(blocks, id) {
				blocks = append(blocks, id)
			}
		}
	}
	for _, index := range placing {
		id := placement.ids[index]
		if ref, written := sources.parent[id]; written {
			add(ref, true, true)
		}
		for _, ref := range sources.serves[id] {
			add(ref, false, true)
		}
		for _, ref := range sources.from[id] {
			add(ref, true, false)
		}
	}
	resolved := authoredResolution{farEnds: farEnds}
	if len(items) > 0 {
		var err error
		if resolved.items, err = conversationTargets(ctx, q, items, document.At); err != nil {
			return authoredResolution{}, err
		}
	}
	starts := keptEnds
	if len(blocks) > 0 {
		rows, err := q.Query(ctx, `
			select d.node_id, coalesce(a.issue_key, '') from document_blocks d
			join artifacts a on a.id = d.artifact_id
			where d.node_id = any($1) and d.node_kind is not null
		`, blocks)
		if err != nil {
			return authoredResolution{}, fmt.Errorf("read the blocks the authored nodes of %s name: %w", document.RefKey, err)
		}
		resolved.blocks = make(map[string]string, len(blocks))
		for rows.Next() {
			var node, issue string
			if err := rows.Scan(&node, &issue); err != nil {
				rows.Close()
				return authoredResolution{}, fmt.Errorf("read a block the authored nodes of %s name: %w", document.RefKey, err)
			}
			resolved.blocks[node] = issue
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return authoredResolution{}, fmt.Errorf("read the blocks the authored nodes of %s name: %w", document.RefKey, err)
		}
		starts = slices.Concat(starts, slices.Collect(maps.Keys(resolved.blocks)))
	}
	if len(starts) > 0 {
		slices.Sort(starts)
		var err error
		if resolved.chains, err = PartOfChains(ctx, q, slices.Compact(starts)); err != nil {
			return authoredResolution{}, fmt.Errorf("read where the blocks the authored nodes of %s stand under stand: %w", document.RefKey, err)
		}
	}
	if document.Issue == "" {
		return resolved, nil
	}
	for _, index := range placing {
		ref, written := sources.parent[placement.ids[index]]
		if !written {
			continue
		}
		target, _ := TargetOf(ref)
		issue, known := resolved.blocks[resolvedFarEnd(target, farEnds).id]
		if item, found := resolved.items[target.ID]; found {
			issue, known = item.Issue, true
		}
		if known && issue != document.Issue {
			var err error
			if resolved.ancestors, err = IssueAncestors(ctx, q, document.Issue); err != nil {
				return authoredResolution{}, err
			}
			break
		}
	}
	return resolved, nil
}

// PartOfChains reads, in one recursive statement, the chain each block node of starts climbs
// through by its current part_of and by no other relation or value (graphstore.CurrentTupleRead,
// through graph_tuples_current), keyed by the block's node id, nearest far end first, each at most
// authoredChainDepth far ends long: the walk up the tree from a block that the block index climbs
// to keep a part_of cycle out of where it places an authored node (authoredPlaces.cycles).
func PartOfChains(ctx context.Context, q Queryer, starts []string) (map[string][]Target, error) {
	rows, err := q.Query(ctx, `
		with recursive chain (start, kind, id, depth) as (
			select s.id, '`+BlockNodeKind+`'::text, s.id, 0 from unnest($1::text[]) as s (id)
			union all
			select chain.start, held.to_kind, held.to_id, chain.depth + 1
			from chain
			cross join lateral (
				`+graphstore.CurrentTupleRead(`t.to_kind, t.to_id`, `t.to_kind, t.to_id, t.asserted`,
		`t.from_kind = chain.kind and t.from_id = chain.id and t.kind = '`+graphstore.TuplePartOf+`'
				  and t.to_kind <> '`+graphstore.KindValue+`'`)+`
			) held
			where held.asserted and chain.depth < $2
		)
		select start, kind, id from chain where depth > 0 order by start, depth
	`, starts, authoredChainDepth)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	chains := make(map[string][]Target, len(starts))
	for rows.Next() {
		var start string
		var far Target
		if err := rows.Scan(&start, &far.Kind, &far.ID); err != nil {
			return nil, err
		}
		chains[start] = append(chains[start], far)
	}
	return chains, rows.Err()
}

// authoredPlaces writes where authored nodes stand into a placement and its projection
// (placeAuthored). index is each block of the placement by its id, with its index among the
// version's blocks, and placed each of those blocks' index in the placement, both made the first
// time a far end names a block of the indexed artifact or the cycle walk climbs a block. outside is
// each block of the indexed issue room that stands in a fragment of a document the pass covers no
// version of (BlockIndexing.outside), nil for a pass over every fragment.
type authoredPlaces struct {
	document  IndexedVersion
	nodes     []model.ArtifactBlock
	placement *blockPlacement
	parentIDs []*string
	index     map[string]int
	placed    map[int]int
	outside   map[string]struct{}
}

// set places the block at index under far: under the block of the version far names, with that
// block's id as its projection; under the document's top with none; and under any other node by
// its far end, with the block's id as its projection where far is a block of the indexed artifact
// the version does not hold.
func (p *authoredPlaces) set(index int, far blockFarEnd) {
	p.placement.parents[index], p.parentIDs[index] = -1, nil
	if p.placement.ends != nil {
		p.placement.ends[index] = nil
	}
	topKind, topID := p.document.top()
	if far.kind == topKind && far.id == topID {
		return
	}
	block, held := p.ownBlock(far)
	switch {
	case held >= 0:
		p.placement.parents[index], p.parentIDs[index] = held, &p.nodes[held].ID
		return
	case block != "":
		p.parentIDs[index] = &block
	}
	p.placement.end(index, far)
}

// indexBlocks makes index and placed where they are not made yet.
func (p *authoredPlaces) indexBlocks() {
	if p.index != nil {
		return
	}
	p.index = make(map[string]int, len(p.placement.placed))
	p.placed = make(map[int]int, len(p.placement.placed))
	for position, node := range p.placement.placed {
		p.index[p.nodes[node].ID], p.placed[node] = node, position
	}
}

// ownBlock is the id of the block of the indexed artifact far names, "" where far is no such
// block, and its index among the version's blocks, -1 where the placement holds none of it.
func (p *authoredPlaces) ownBlock(far blockFarEnd) (string, int) {
	if far.kind != BlockNodeKind {
		return "", -1
	}
	id, own := strings.CutPrefix(far.id, p.document.RefKey+"#")
	if !own {
		return "", -1
	}
	p.indexBlocks()
	if node, held := p.index[id]; held {
		return id, node
	}
	return id, -1
}

// cycles reports whether the part_of chain the authored node at index climbs through returns to
// it, as this pass places the blocks of the version and as the store holds every other node: up
// the parent this pass gives each block of the version, and from a block no version of this pass
// holds - another artifact's, or one of the indexed artifact the version dropped - up its chain as
// the store holds it (chains), back into the placement at the first block of the version that
// chain reaches, since the pass states its own placement of that block rather than the store's.
// The walk ends, answering false, at the document's top; at a node that is no block - an issue,
// a conversation node, an answered ask - none of which stands under a block of a document; at the
// end of a chain; and after as many steps as the placement holds blocks, the length of the
// longest walk that passes no block twice, so a cycle the node is not on ends it too.
func (p *authoredPlaces) cycles(index int, chains map[string][]Target) bool {
	node := p.placement.placed[index]
	p.indexBlocks()
	position := index
	for range len(p.placement.ids) {
		if parent := p.placement.parents[position]; parent >= 0 {
			if parent == node {
				return true
			}
			next, placed := p.placed[parent]
			if !placed {
				return false
			}
			position = next
			continue
		}
		if p.placement.ends == nil || p.placement.ends[position] == nil || p.placement.ends[position].kind != BlockNodeKind {
			return false
		}
		climbed := false
		for _, far := range chains[p.placement.ends[position].id] {
			_, held := p.ownBlock(blockFarEnd{kind: far.Kind, id: far.ID})
			if held < 0 {
				continue
			}
			if held == node {
				return true
			}
			next, placed := p.placed[held]
			if !placed {
				return false
			}
			position, climbed = next, true
			break
		}
		if !climbed {
			return false
		}
	}
	return false
}

// candidates is every parent the authored node at index may take, in the precedence's order
// (placeAuthored): its parent attribute's target where that is a conversation node, or a block of
// a document, of the node's own issue or of an issue above it; then each target of its serves
// pointer, in written order, that is a block of a document of its own issue; then, for a
// requirement, each reference of its From: line that is a conversation node of its own issue;
// and last its issue, or the document's top for a document no issue holds, which every node may
// take. A block of the indexed artifact is a candidate only where the version holds it. The node
// takes the first candidate its chain does not climb back to it from (authoredPlaces.cycles).
func (p *authoredPlaces) candidates(index int, sources authoredSources, resolved authoredResolution) []blockFarEnd {
	id, node := p.placement.ids[index], p.placement.placed[index]
	var candidates []blockFarEnd
	if ref, written := sources.parent[id]; written {
		if far, ok := p.target(ref, resolved, true); ok {
			candidates = append(candidates, far)
		}
	}
	for _, ref := range sources.serves[id] {
		if far, ok := p.target(ref, resolved, false); ok {
			candidates = append(candidates, far)
		}
	}
	if issue := p.document.Issue; issue != "" && p.nodes[node].Type == requirementType {
		for _, ref := range sources.from[id] {
			target, ok := TargetOf(ref)
			if !ok || pmdoc.ItemKindOf(ref) == "" || !graphstore.AddressFits(target.ID) {
				continue
			}
			if item, found := resolved.items[target.ID]; found && item.Conversation && item.Issue == issue {
				candidates = append(candidates, blockFarEnd{kind: target.Kind, id: target.ID})
			}
		}
	}
	if p.document.Issue != "" {
		return append(candidates, blockFarEnd{kind: graphstore.KindIssue, id: p.document.Issue})
	}
	kind, topID := p.document.top()
	return append(candidates, blockFarEnd{kind: kind, id: topID})
}

// target is the far end ref names where an authored node may hang under it: for its parent
// attribute (above), a conversation node, or a block of a document, of the node's own issue or of
// an issue above it; for a serves target, a block of a document of its own issue. A block is the
// node its address resolves to (resolved.farEnds). A block of the indexed artifact is one only
// where the version holds it, since the pass places no other.
func (p *authoredPlaces) target(ref text.Ref, resolved authoredResolution, above bool) (blockFarEnd, bool) {
	target, ok := TargetOf(ref)
	if !ok {
		return blockFarEnd{}, false
	}
	far := resolvedFarEnd(target, resolved.farEnds)
	if !graphstore.AddressFits(far.id) {
		return blockFarEnd{}, false
	}
	var issue string
	switch {
	case pmdoc.ItemKindOf(ref) != "":
		item, found := resolved.items[target.ID]
		if !above || !found || !item.Conversation {
			return blockFarEnd{}, false
		}
		issue = item.Issue
	case target.Kind == BlockNodeKind:
		if block, held := p.ownBlock(far); block != "" {
			// A block of another fragment of the room stands, though this pass places none of it.
			_, stands := p.outside[block]
			return far, held >= 0 || stands
		}
		held, found := resolved.blocks[far.id]
		if !found {
			return blockFarEnd{}, false
		}
		issue = held
	default:
		return blockFarEnd{}, false
	}
	if issue != "" && (issue == p.document.Issue || above && slices.Contains(resolved.ancestors, issue)) {
		return far, true
	}
	return blockFarEnd{}, false
}

// The values the block index states of every block beside its parent (R1 and R15 of
// the issue-tree spec, D1 and D9 of its plan): AttributeRank, its place among its siblings, a
// base-62 string compared byte for byte (`collate "C"`), which stands in its document's band
// (RankBandSpec and the rest); graphstore.AttributeText, the block's whole slice of its version's
// markdown, its children's text included, which document_block_texts projects; and
// AttributePlacement, which says what decides the block's parent: PlacementText, the text it
// stands in, for every block the rendering places, PlacementAuthored for an authored node, whose
// parent its author decides (placeAuthored), and PlacementStored for a block a move of the node
// route placed (R7 of the issue-tree spec: api's PATCH /api/v1/issues/{key}/nodes/{id}), whose
// part_of and rank the store holds as that move wrote them and no pass computes from the text, but
// for the exceptions planBlockParents and placeAuthored name: a removed parent, an edited parent
// attribute, and a decision that settles an ask, which stands under it. A plan node holds more: a
// value of each attribute its opening line writes that points at nothing, under the attribute's
// name, its `size` (AttributePlanSize) in whole minutes (blockPlanValues).
const (
	AttributeRank      = "rank"
	AttributePlacement = "placement"
	PlacementText      = "text"
	PlacementAuthored  = "authored"
	PlacementStored    = "stored"
)

// A block's place in its document's text, which no rank holds: a rank orders a block only among
// the children of its tree parent (rankBlocks), and an authored node's tree parent is the one its
// author names, not the block whose text holds it. So every block that is a node also carries two
// dated facts of the text. TupleTextParent is a tuple to the member of
// its document whose text holds it: the tightest block of the version that holds it and is a node
// (placeBlocks, before placeAuthored), or, for a block no such block holds, the node of the
// document its in_document names. AttributeTextRank is its place among the blocks that share its
// text parent, in the version's text order, a rank string of its document's band kept and given
// as a rank is (childRanker.rank). Neither is a part_of or a placement: no walk of the tree reads
// either, and a version of a document renders from them (docs' RenderAt). graph_edges renders no
// text_parent, since its tuple arm answers a tuple out of a block other than part_of only where a
// block_refs row of that node, kind and far end stands.
const (
	TupleTextParent   = "text_parent"
	AttributeTextRank = "text_rank"
)

// AttributePlanSize is the value a plan node says how big it is by: the plan_task attribute
// `size`, a duration its author wrote in hours and minutes (`45m`, `2h`, `1h30m`), which the node
// holds as its whole number of minutes (pmdoc.Minutes), a value of the node since it points at
// nothing, and absent where its author wrote none (blockPlanValues). An artifact's `size`
// (graphstore.AttributeSize) is its bytes, on a node of another kind.
const AttributePlanSize = "size"

// WrittenSizeSQL is a plan node's size value read back off the store rather than off a render: as
// SQL over the document_blocks row alias, the size its block's opening line writes in the newest
// text its document_block_texts rows hold - the `size="..."` of that text's first line, which the
// renderer writes in that form (pmdoc's renderTypedAttributes), and which no other attribute's
// value can hold, since the renderer writes a quote inside a value as a character reference - in
// whole minutes, as the text of the number the node holds (blockPlanValues), for a plan_task, and
// null for a block of any other type, for one whose line writes none and for one no row holds a
// text of. It reads the hours before `h` and the minutes before a closing `m`, as pmdoc.Minutes
// reads a duration the schema took. It is the row side of the census of a plan node's size
// (graphcensus' block_size).
func WrittenSizeSQL(alias string) string {
	return `(case when ` + alias + `.type = '` + planTaskType + `' then (
			select (coalesce(substring(opened.size from '^([0-9]+)h'), '0')::numeric * 60
			        + coalesce(substring(opened.size from '([0-9]+)m$'), '0')::numeric)::text
			from (select substring(split_part((
				select written.text from document_block_texts written
				where written.artifact_id = ` + alias + `.artifact_id and written.block_id = ` + alias + `.block_id and written.text is not null
				order by written.version desc limit 1), chr(10), 1) from ' ` + AttributePlanSize + `="([^"]*)"') as size) opened
			where opened.size is not null) end)`
}

// heldBlockValue is what the store holds now of one block the values pass reads: its current
// rank, "" where it holds none, whether it holds a current text value, its current placement, ""
// where it holds none, its current text_rank, "" where it holds none, the far end of its current
// text_parent, the zero far end where it states none, and the current value of each attribute of
// a plan node it holds (planValueNames) by name, nil where it holds none.
type heldBlockValue struct {
	rank, placement, textRank string
	text                      bool
	textParent                blockFarEnd
	plan                      map[string]string
}

// planBlockValues plans the rank, the text, the placement, the place in the text (a text_parent
// and a text_rank) and, for a plan node, the value of each attribute that points at nothing
// (blockPlanValues) of every block placement placed, dated at the pass's own time
// (IndexedVersion.At). It answers the values it states, which PlanBlocks appends with the pass's
// other facts, and what the blocks hold once the indexing is written, for a later indexing of the
// document in the same transaction (Held).
//
// It reads what the store holds of every one of those blocks in one statement (heldBlockValues),
// in the plan's read phase, and states a value only where it moves: a rank where the block's place
// among its siblings moved (rankBlocks), a text where its text sum changed - changed is the blocks
// whose document_blocks row the pass writes, so the text value and the document_block_texts row
// move together - or where the block holds no current text yet, and a placement where the block
// holds none or another: PlacementAuthored for an authored node (placeAuthored), PlacementText for
// every other block. A text_parent is stated where the block's text parent (placement.textParents,
// textParentHeld) is not the one it holds, with the retraction of the one it holds where it holds
// one, and a text_rank where its place among the blocks sharing its text parent moved
// (textRankBlocks); neither moves a changed_version or a document_block_texts row. A plan node's
// value is stated under the attribute's name where the block's opening line writes one the node
// does not hold (blockPlanValues): the text the line writes, and the size in whole minutes. The
// value the node holds of an attribute of planValueNames is retracted where the line writes none -
// its author took it out, or the block is no plan node any more - so a block that holds none and
// writes none states nothing. So a block a version only moved takes its new parent, its new rank
// and its new place in the text here although the pass writes no row of it, a re-index of a
// document nothing moved states nothing, and the first index of a document, or the first pass to
// reach a block an earlier build indexed, gives every block its rank, its text, its placement, its
// text_parent, its text_rank and any plan value it writes. A block the version dropped is no block
// the pass places: it keeps the values it held, and its node, stated gone, says it is no part of
// the version. A block an earlier indexing of this transaction placed (BlockIndex.values) is
// compared with what that indexing stated of it, which the store holds only once the write phase
// has run.
//
// stale is the BlockIndex's stale stand: another release indexed the document after the store
// last stated its blocks, and rewrote the rows and the document_block_texts of each block whose
// text its version moved while stating no text value. The sums the pass compared the render with
// are that release's, so a block whose text that release moved is in no changed set, and its text
// value still says what the store last stated. Such a pass states the text of every block it
// places, as the first index of a document does.
func planBlockValues(
	ctx context.Context,
	q Queryer,
	indexed BlockIndex,
	document IndexedVersion,
	markdown string,
	nodes []model.ArtifactBlock,
	placement blockPlacement,
	changed map[string]struct{},
	stale bool,
	fragments map[string]fragmentDocument,
) ([]graphstore.UncausedTuple, map[string]heldBlockValue, error) {
	standing := maps.Clone(indexed.values)
	if len(placement.ids) == 0 {
		return nil, standing, nil
	}
	held := make([]heldBlockValue, len(placement.ids))
	names := planValueNames()
	var unread []string
	var unreadAt []int
	for index, id := range placement.ids {
		if value, known := indexed.values[id]; known {
			held[index] = value
			continue
		}
		unread, unreadAt = append(unread, id), append(unreadAt, index)
	}
	if len(unread) > 0 {
		read, err := heldBlockValues(ctx, q, document.RefKey, unread, names)
		if err != nil {
			return nil, nil, fmt.Errorf("read the values the blocks of %s hold: %w", document.RefKey, err)
		}
		for position, index := range unreadAt {
			held[index] = read[position]
		}
	}
	// A block a move of this transaction placed holds what the move states of it, which the store
	// holds only once the write phase has run: its rank, and the placement PlacementStored.
	keeps := make([]bool, len(placement.ids))
	for index, id := range placement.ids {
		if move, moved := indexed.storedMove(blockNodeAddress(document.RefKey, id)); moved {
			held[index].rank, held[index].placement = move.Rank, PlacementStored
		}
		keeps[index] = placement.keepsStored(index, held[index].placement)
	}
	if standing == nil {
		standing = make(map[string]heldBlockValue, len(placement.ids))
	}
	bands := blockBands(document, nodes, fragments)
	ranks := rankBlocks(bands, placement, held, keeps)
	if err := spaceFromStoredRanks(ctx, q, indexed, document, nodes, placement, held, keeps, bands, ranks); err != nil {
		return nil, nil, err
	}
	textRanks := textRankBlocks(bands, document, nodes, placement, fragments, held)
	var tuples []graphstore.UncausedTuple
	value := func(id, attribute, to string, asserted bool) {
		tuples = append(tuples, graphstore.UncausedTuple{Tuple: valueTuple(blockNodeAddress(document.RefKey, id), attribute, to, document.At, document.Writer, asserted)})
	}
	textParent := func(id string, far blockFarEnd, asserted bool) {
		tuples = append(tuples, graphstore.UncausedTuple{Tuple: graphstore.Tuple{
			FromKind: BlockNodeKind, FromID: blockNodeAddress(document.RefKey, id), Kind: TupleTextParent,
			ToKind: far.kind, ToID: far.id,
			At: document.At, Who: document.Writer, Asserted: asserted,
		}})
	}
	for index, id := range placement.ids {
		block := nodes[placement.placed[index]]
		if ranks[index] != held[index].rank {
			value(id, AttributeRank, ranks[index], true)
		}
		if _, wrote := changed[id]; wrote || stale || !held[index].text {
			value(id, graphstore.AttributeText, markdown[block.From:block.To], true)
		}
		placed := PlacementText
		switch {
		case keeps[index]:
			placed = PlacementStored
		case authoredType(block.Type):
			placed = PlacementAuthored
		}
		if held[index].placement != placed {
			value(id, AttributePlacement, placed, true)
		}
		// The text parent the block holds stands where it is the one the text gives it, which is
		// read without building its address; only one that moved builds the address it states.
		holder, inDocument := "", document.inDocument(block, fragments)
		if parent := placement.textParents[index]; parent >= 0 {
			holder = nodes[parent].ID
		}
		standsUnder := held[index].textParent
		if !textParentHeld(standsUnder, document.RefKey, holder, inDocument) {
			if standsUnder.kind != "" {
				textParent(id, standsUnder, false)
			}
			standsUnder = blockFarEnd{kind: graphstore.KindArtifact, id: inDocument}
			if holder != "" {
				standsUnder = blockFarEnd{kind: BlockNodeKind, id: blockNodeAddress(document.RefKey, holder)}
			}
			textParent(id, standsUnder, true)
		}
		if textRanks[index] != held[index].textRank {
			value(id, AttributeTextRank, textRanks[index], true)
		}
		written, err := blockPlanValues(block)
		if err != nil {
			return nil, nil, fmt.Errorf("read the values of %s: %w", document.RefKey, err)
		}
		for _, name := range names {
			stated, writes := written[name]
			holding, holds := held[index].plan[name]
			switch {
			case writes && holds && stated == holding:
			case writes:
				value(id, name, stated, true)
			case holds:
				value(id, name, holding, false)
			}
		}
		standing[id] = heldBlockValue{
			rank: ranks[index], placement: placed, textRank: textRanks[index], text: true, textParent: standsUnder, plan: written,
		}
	}
	return tuples, standing, nil
}

// textParentHeld says far, the text parent a block holds, is the one the text gives it: the block
// holder of the version, by its address under refKey, or, where holder is "", the node of the
// document inDocument, the ref_key its in_document names. The address is compared piece by piece,
// so a pass over a document of thousands of blocks builds none for a block that did not move.
func textParentHeld(far blockFarEnd, refKey, holder, inDocument string) bool {
	if holder == "" {
		return far.kind == graphstore.KindArtifact && far.id == inDocument
	}
	return far.kind == BlockNodeKind && len(far.id) == len(refKey)+1+len(holder) &&
		strings.HasPrefix(far.id, refKey) && far.id[len(refKey)] == '#' && strings.HasSuffix(far.id, holder)
}

// heldBlockValues reads, in one statement, the current rank, text, placement and text_rank of each
// block of ids, and the current value of each attribute of a plan node that names names
// (planValueNames), as graphstore.CurrentValue reads one value - the newest tuple under the
// attribute's name, kept while it asserts - through graph_tuples_values, one probe a value, and
// its current text_parent as heldAuthoredBlocks reads a part_of, the newest tuple per far end kept
// while it asserts (graphstore.CurrentTupleRead), through graph_tuples_current. Each lateral of a
// value writes to_kind = 'value' into its text rather than binding it, as CurrentValue does,
// because graph_tuples_values is partial on that predicate: a bound kind is a parameter the
// planner of a generic plan cannot match the index's predicate against. Of a block's text it reads
// whether one is held and not the text itself, which no comparison here needs. A block's plan
// values come back as one object of the values it holds, by name, and none where it holds none, as
// every block but a plan node holds. It answers the blocks in ids' order, and names each by its
// id, building its address from refKey as every other statement of the pass does.
func heldBlockValues(ctx context.Context, q Queryer, refKey string, ids, names []string) ([]heldBlockValue, error) {
	rows, err := q.Query(ctx, `
		select coalesce(rank.held, ''), coalesce(text.held, false), coalesce(placement.held, ''),
		       coalesce(text_rank.held, ''), coalesce(text_parent.to_kind, ''), coalesce(text_parent.to_id, ''), plan.held
		from unnest($1::text[]) with ordinality as block (id, position)
		left join lateral (
			select case when t.asserted then t.to_id end as held from graph_tuples t
			where t.from_kind = $2 and t.from_id = $3 || '#' || block.id and t.kind = $4
			  and t.to_kind = '`+graphstore.KindValue+`'
			order by t.seq desc limit 1
		) rank on true
		left join lateral (
			select t.asserted as held from graph_tuples t
			where t.from_kind = $2 and t.from_id = $3 || '#' || block.id and t.kind = $5
			  and t.to_kind = '`+graphstore.KindValue+`'
			order by t.seq desc limit 1
		) text on true
		left join lateral (
			select case when t.asserted then t.to_id end as held from graph_tuples t
			where t.from_kind = $2 and t.from_id = $3 || '#' || block.id and t.kind = $6
			  and t.to_kind = '`+graphstore.KindValue+`'
			order by t.seq desc limit 1
		) placement on true
		left join lateral (
			select case when t.asserted then t.to_id end as held from graph_tuples t
			where t.from_kind = $2 and t.from_id = $3 || '#' || block.id and t.kind = $7
			  and t.to_kind = '`+graphstore.KindValue+`'
			order by t.seq desc limit 1
		) text_rank on true
		left join lateral (
			select held.to_kind, held.to_id from (
				`+graphstore.CurrentTupleRead(`t.to_kind, t.to_id`, `t.to_kind, t.to_id, t.asserted, t.seq`,
		`t.from_kind = $2 and t.from_id = $3 || '#' || block.id and t.kind = $8
				  and t.to_kind <> '`+graphstore.KindValue+`'`)+`
			) held
			where held.asserted
			order by held.seq desc limit 1
		) text_parent on true
		left join lateral (
			select jsonb_object_agg(attribute.name, newest.to_id) as held
			from unnest($9::text[]) as attribute (name)
			cross join lateral (
				select t.to_id, t.asserted from graph_tuples t
				where t.from_kind = $2 and t.from_id = $3 || '#' || block.id and t.kind = attribute.name
				  and t.to_kind = '`+graphstore.KindValue+`'
				order by t.seq desc limit 1
			) newest
			where newest.asserted
		) plan on true
		order by block.position
	`, ids, BlockNodeKind, refKey, AttributeRank, graphstore.AttributeText, AttributePlacement,
		AttributeTextRank, TupleTextParent, names)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	held := make([]heldBlockValue, 0, len(ids))
	for rows.Next() {
		var value heldBlockValue
		if err := rows.Scan(&value.rank, &value.text, &value.placement,
			&value.textRank, &value.textParent.kind, &value.textParent.id, &value.plan); err != nil {
			return nil, err
		}
		held = append(held, value)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(held) != len(ids) {
		return nil, fmt.Errorf("read the values of %d blocks and got %d back", len(ids), len(held))
	}
	return held, nil
}

// rankBlocks is the rank each block placement placed stands at once the pass has run, beside it
// in placement's order. held is what the store holds of each (heldBlockValues), and bands the band
// of each of the version's unique blocks (blockBands), the indexes placement.parents ranges over.
//
// The children of one parent are this document's blocks that parent holds, in document order:
// the blocks of the document's top level, the blocks one block holds, or the blocks one node that
// is no block of the version holds (placement.ends: the readings an answered ask holds, or the
// authored nodes a conversation node, a block of another document or an issue holds). Each
// parent's children are ranked on their own, and only among themselves, inside the document's
// band (rankChildren), so the children another document hangs under the same parent - the issue's
// other documents' top-level blocks, or the checks and the plan nodes under one requirement - keep
// the ranks their own documents' passes gave them, and stand before or after these by their bands.
// An issue room's top level holds several documents' fragments, each a run of children of one
// band in the room's order, and each run is ranked inside its own band.
//
// A block that keeps its stored placement (keeps, planBlockValues) keeps the rank the store holds
// too: it is ranked on its own, as the one child of a parent of its own, which keeps any rank that
// stands in its band, and it takes no part in its siblings' ranking, whose text it does not follow
// (spaceFromStoredRanks keeps their new ranks off it).
func rankBlocks(bands []byte, placement blockPlacement, held []heldBlockValue, keeps []bool) []string {
	nodes := len(bands)
	// A parent is the index of the block holding the child plus one, 0 for the document's top,
	// or past nodes+1 for a node that is no block of the version, or for a stored block alone.
	parents := nodes + 1
	var ends map[blockFarEnd]int
	parentOf := make([]int, len(placement.ids))
	alone := 0
	for index := range placement.ids {
		parentOf[index] = placement.parents[index] + 1
		if placement.ends != nil && placement.ends[index] != nil {
			if ends == nil {
				ends = map[blockFarEnd]int{}
			}
			end, seen := ends[*placement.ends[index]]
			if !seen {
				end = nodes + 1 + len(ends)
				ends[*placement.ends[index]] = end
			}
			parentOf[index] = end
		}
	}
	parents += len(ends)
	for index := range placement.ids {
		if keeps[index] {
			parentOf[index] = parents + alone
			alone++
		}
	}
	parents += alone
	return rankChildren(bands, placement.placed, parentOf, parents, func(child int) string { return held[child].rank })
}

// spaceFromStoredRanks keeps each new rank rankBlocks gave a block placed by its text off every
// rank its parent's other children hold (R7 of the issue-tree spec): a stored block's, which
// the pass leaves where it stands, and those of another document's or another room's children of
// the same parent. It reads, for each parent a new rank goes under, the rank of every child the
// store holds under it, in one statement (parentChildRanks) - one read more for each parent the
// pass writes - beside the ranks a move of this transaction gave its blocks
// (BlockIndex.storedMove). A new rank that one of them holds moves to rank.Between it and the
// next rank above it among the parent's children, or the band's own upper bound, so it equals
// none of them and stays below every later sibling. A rank the block already held is no new rank
// and stays where it is.
func spaceFromStoredRanks(ctx context.Context, q Queryer, indexed BlockIndex, document IndexedVersion, nodes []model.ArtifactBlock, placement blockPlacement, held []heldBlockValue, keeps []bool, bands []byte, ranks []string) error {
	topKind, topID := document.top()
	parentOf := func(index int) blockFarEnd {
		switch {
		case placement.ends != nil && placement.ends[index] != nil:
			return *placement.ends[index]
		case placement.parents[index] >= 0:
			return blockFarEnd{kind: BlockNodeKind, id: blockNodeAddress(document.RefKey, nodes[placement.parents[index]].ID)}
		}
		return blockFarEnd{kind: topKind, id: topID}
	}
	// The parents a new rank goes under: a rank the block already held is none.
	var written []blockFarEnd
	wrote := map[blockFarEnd]bool{}
	for index := range placement.ids {
		if keeps[index] || ranks[index] == held[index].rank {
			continue
		}
		if parent := parentOf(index); !wrote[parent] {
			wrote[parent] = true
			written = append(written, parent)
		}
	}
	if len(written) == 0 {
		return nil
	}
	children, err := parentChildRanks(ctx, q, written)
	if err != nil {
		return fmt.Errorf("read the ranks of the children the parents of %s hold: %w", document.RefKey, err)
	}
	// Each placed block by its address: one placed by its text takes its rank here, so what the
	// store holds of it is no other child's rank.
	placed := make(map[string]int, len(placement.ids))
	for index, id := range placement.ids {
		placed[blockNodeAddress(document.RefKey, id)] = index
	}
	others := map[blockFarEnd][]string{}
	for _, child := range children {
		if index, ours := placed[child.address]; ours && !keeps[index] {
			continue
		}
		if _, moved := indexed.storedMove(child.address); moved {
			continue
		}
		others[child.parent] = append(others[child.parent], child.rank)
	}
	for _, move := range indexed.stored {
		if parent := (blockFarEnd{kind: move.ParentKind, id: move.ParentID}); wrote[parent] && move.Rank != "" {
			others[parent] = append(others[parent], move.Rank)
		}
	}
	// The parent of every placed block, built at the first new rank that collides, which is the
	// first that walks its siblings, rather than at each walk.
	var parents []blockFarEnd
	for index := range placement.ids {
		if keeps[index] || ranks[index] == held[index].rank {
			continue
		}
		parent := parentOf(index)
		if !slices.Contains(others[parent], ranks[index]) {
			continue
		}
		if parents == nil {
			parents = make([]blockFarEnd, len(placement.ids))
			for sibling := range placement.ids {
				parents[sibling] = parentOf(sibling)
			}
		}
		upper := string(bands[placement.placed[index]] + 1)
		consider := func(candidate string) {
			if candidate > ranks[index] && candidate < upper {
				upper = candidate
			}
		}
		for _, other := range others[parent] {
			consider(other)
		}
		for sibling := range placement.ids {
			if sibling != index && !keeps[sibling] && parents[sibling] == parent {
				consider(ranks[sibling])
			}
		}
		ranks[index] = rank.Between(ranks[index], upper)
	}
	return nil
}

// parentChild is one child a parent holds now, as parentChildRanks reads it: the parent, the
// child's node address, and the rank the child holds.
type parentChild struct {
	parent  blockFarEnd
	address string
	rank    string
}

// parentChildRanks reads, in one statement, every block each of parents holds now through its
// current part_of (graphstore.CurrentTupleRead, into a node, which graph_tuples_as_of_in serves:
// the read names to_kind <> 'value', the index's predicate, which a far end bound to a column of
// the probe does not prove), with the rank it holds now, read as heldBlockValues reads a value; a
// child that holds no rank, or that is no block, is left out.
//
// Each parent's probe names the parent and the relation's kind alone, behind an offset 0 the
// planner can neither flatten nor push a test through, and the test that a child is a block
// stands outside it. Inside the probe that test bounds the first key of graph_tuples_current,
// whose order is the probe's own, and a plan made on the statistics of a graph_tuples that held
// no tuple out of a block - a store analyzed before its first block was indexed - counts that
// key's range empty and walks it for each parent: every current tuple out of every block the
// store holds, a cost that grows with the parents times the store. Measured on 2026-11-26 on the
// build farm, over six rooms the reconciliation indexed one after another in one database whose
// graph_tuples was analyzed before the first: the read of 12,625 parents hit 27,194,250 buffers
// in 112.6 s of a pass of 129 s over 20,000 blocks, and that of 2,953 parents 12,772,954 buffers
// in 52.0 s of a pass of 55.6 s over 4,682. A probe a parent costs the same whatever the
// statistics say (TestTheParentChildRanksReadProbesEachParentByItsFarEnd).
func parentChildRanks(ctx context.Context, q Queryer, parents []blockFarEnd) ([]parentChild, error) {
	kinds, ids := make([]string, len(parents)), make([]string, len(parents))
	for index, parent := range parents {
		kinds[index], ids[index] = parent.kind, parent.id
	}
	rows, err := q.Query(ctx, `
		select parent.kind, parent.id, child.from_id, ranked.held
		from unnest($1::text[], $2::text[]) as parent (kind, id)
		join lateral (
			select held.from_id from (
				`+graphstore.CurrentTupleRead(`t.from_kind, t.from_id`, `t.from_kind, t.from_id, t.asserted`,
		`t.to_kind = parent.kind and t.to_id = parent.id and t.kind = $3
				  and t.to_kind <> '`+graphstore.KindValue+`'`)+`
				offset 0
			) held
			where held.asserted and held.from_kind = $4
		) child on true
		join lateral (
			select case when t.asserted then t.to_id end as held from graph_tuples t
			where t.from_kind = $4 and t.from_id = child.from_id and t.kind = $5
			  and t.to_kind = '`+graphstore.KindValue+`'
			order by t.seq desc limit 1
		) ranked on ranked.held is not null
	`, kinds, ids, graphstore.TuplePartOf, BlockNodeKind, AttributeRank)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var children []parentChild
	for rows.Next() {
		var child parentChild
		if err := rows.Scan(&child.parent.kind, &child.parent.id, &child.address, &child.rank); err != nil {
			return nil, err
		}
		children = append(children, child)
	}
	return children, rows.Err()
}

// textRankBlocks is the text_rank each block placement placed stands at once the pass has run,
// beside it in placement's order: its place among the blocks that share its text parent
// (placement.textParents), in document order, ranked inside its document's band as rankBlocks ranks
// one parent's children (rankChildren), so a text_rank is kept where it already stands in order
// and takes rank.Between its kept neighbours elsewhere. held is what the store holds of each
// (heldBlockValues). The blocks no block of the version holds share the node of the document their
// in_document names (IndexedVersion.inDocument), so an issue room's top level is ranked fragment by
// fragment, each among its own document's blocks.
func textRankBlocks(bands []byte, document IndexedVersion, nodes []model.ArtifactBlock, placement blockPlacement, fragments map[string]fragmentDocument, held []heldBlockValue) []string {
	// A text parent is the index of the block holding the child plus one, or past len(nodes)
	// for a document's node, one for each document whose top level the version holds.
	var documents map[string]int
	parentOf := make([]int, len(placement.ids))
	for index, parent := range placement.textParents {
		if parent >= 0 {
			parentOf[index] = parent + 1
			continue
		}
		in := document.inDocument(nodes[placement.placed[index]], fragments)
		top, seen := documents[in]
		if !seen {
			if documents == nil {
				documents = map[string]int{}
			}
			top = len(nodes) + 1 + len(documents)
			documents[in] = top
		}
		parentOf[index] = top
	}
	return rankChildren(bands, placement.placed, parentOf, len(nodes)+1+len(documents), func(child int) string { return held[child].textRank })
}

// rankChildren is the rank of each child of parentOf, beside it: each one's parent, of parents
// counted from 0, in document order. placed is each child's index among the version's unique
// blocks, whose band bands holds, and held the rank each child holds now. Each parent's children
// are ranked on their own, and only among themselves (childRanker.rank), each run of children of
// one band inside that band. Each parent's children are a list threaded through next, so no map of
// slices is built for a document of thousands of blocks.
func rankChildren(bands []byte, placed, parentOf []int, parents int, held func(child int) string) []string {
	ranks := make([]string, len(parentOf))
	first, last := make([]int, parents), make([]int, parents)
	next := make([]int, len(parentOf))
	for parent := range first {
		first[parent], last[parent] = -1, -1
	}
	for index, parent := range parentOf {
		next[index] = -1
		if last[parent] < 0 {
			first[parent] = index
		} else {
			next[last[parent]] = index
		}
		last[parent] = index
	}
	var ranker childRanker
	for parent := range first {
		if first[parent] < 0 {
			continue
		}
		ranker.children = ranker.children[:0]
		band := byte(0)
		for child := first[parent]; child >= 0; child = next[child] {
			if childBand := bands[placed[child]]; childBand != band && len(ranker.children) > 0 {
				ranker.rank(band, held, ranks)
				ranker.children = ranker.children[:0]
			}
			band = bands[placed[child]]
			ranker.children = append(ranker.children, child)
		}
		ranker.rank(band, held, ranks)
	}
	return ranks
}

// fragmentDocument is what the block index reads of a document whose fragment of an issue room
// holds a block it indexes under the room's tree artifact (model.ArtifactBlock's Document): the
// ref_key the block's in_document names, and the band its rank stands in.
type fragmentDocument struct {
	refKey string
	band   byte
}

// fragmentDocuments reads, in one statement, each document other than artifactID that a block of
// nodes names as the document whose fragment holds it: none for a document that is its own room,
// whose blocks name none. A document the reading transaction creates, whose row no read sees
// before its write phase, is the address that transaction registered for it (Pending).
func fragmentDocuments(ctx context.Context, q Queryer, artifactID string, nodes []model.ArtifactBlock) (map[string]fragmentDocument, error) {
	documents := map[string]fragmentDocument{}
	pending := pendingOf(ctx)
	var ids []string
	for _, block := range nodes {
		if block.Document == "" || block.Document == artifactID || slices.Contains(ids, block.Document) {
			continue
		}
		if _, held := documents[block.Document]; held {
			continue
		}
		if pending != nil {
			if address, created := pending.PendingDocument(block.Document); created {
				documents[block.Document] = fragmentDocument{refKey: address.RefKey, band: address.Band}
				continue
			}
		}
		ids = append(ids, block.Document)
	}
	if len(ids) == 0 {
		return documents, nil
	}
	rows, err := q.Query(ctx, `
		select a.id::text, a.ref_key, `+DocumentBandSQL("a")+`
		from artifacts a where a.id = any($1::uuid[])
	`, ids)
	if err != nil {
		return nil, fmt.Errorf("read the documents whose fragments of %s's room hold its blocks: %w", artifactID, err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, band string
		var document fragmentDocument
		if err := rows.Scan(&id, &document.refKey, &band); err != nil {
			return nil, fmt.Errorf("read the documents whose fragments of %s's room hold its blocks: %w", artifactID, err)
		}
		document.band = band[0]
		documents[id] = document
	}
	return documents, rows.Err()
}

// inDocument is the ref_key block's in_document names: the document whose fragment holds it where
// it names one the pass read (fragmentDocuments), else the indexed document's own.
func (record IndexedVersion) inDocument(block model.ArtifactBlock, fragments map[string]fragmentDocument) string {
	if held, ok := fragments[block.Document]; ok {
		return held.refKey
	}
	return record.RefKey
}

// blockBands is the band each block of nodes stands in: its fragment's document's
// (fragmentDocuments), else the indexed document's own.
func blockBands(document IndexedVersion, nodes []model.ArtifactBlock, fragments map[string]fragmentDocument) []byte {
	bands := make([]byte, len(nodes))
	for index, block := range nodes {
		bands[index] = document.Band
		if held, ok := fragments[block.Document]; ok {
			bands[index] = held.band
		}
	}
	return bands
}

// planRoomBlocks plans issue_room_blocks, the projection of each in_document of an issue room's
// blocks (D16 of the issue-tree plan) and the one record of the document whose fragment holds each
// block of the room, level with the version the pass indexes under the issue's tree artifact, the
// indexing's own artifact; queueRoomBlocks writes it in the pass's write phase. The pass reads the
// room whole, so the row of every block it does not hold goes: one the version dropped, whose
// document_blocks row goes with it, and one the typed-node pass above the deferral bound recorded
// for a block that went before any pass indexed it (DecisionIndexing's room rows). Each block the
// version holds names its fragment's document (fragmentDocuments), or the tree artifact for a
// block of the tree's own fragment, and a block whose document moved - a block cut from one
// fragment and pasted into another - takes one retraction of the in_document it held and one
// assertion of the new one, dated at the pass's own time, which it answers for PlanBlocks to append
// with the pass's other facts. A block whose row the pass inserts states its in_document where its
// document_blocks row is inserted (PlanBlocks). The blocks of pastBound, whose ids are past
// text.MaxBlockIDBytes, are no nodes and have no document_blocks row, but the room records their
// documents as it does every other block's (the table is keyed through block_id_key, migration
// 20261118230000_issue_tree_room_blocks_key), so the ask of such a block reads the document holding
// it (AskBlockDocumentSQL); such a row states no in_document, and goes once the version holds the
// block no more. A document that is not an issue's tree writes nothing here.
//
// The documents the room records its blocks in are read through q, in the plan's read phase, or,
// where an earlier indexing of this transaction planned the room (BlockIndex.rooms), taken as that
// indexing left them, which the table holds only once the write phase has run. It also answers the
// room's record as the indexing leaves it, for a later indexing of the room in the transaction.
func (b *BlockIndexing) planRoomBlocks(
	ctx context.Context,
	q Queryer,
	indexed BlockIndex,
	document IndexedVersion,
	nodes []model.ArtifactBlock,
	fits []bool,
	pastBound []model.ArtifactBlock,
	fragments map[string]fragmentDocument,
) ([]graphstore.UncausedTuple, map[string]string, error) {
	if document.Band != RankBandTree {
		return nil, indexed.rooms, nil
	}
	artifactID := b.artifactID
	ids := make([]string, 0, len(nodes)+len(pastBound))
	documents := make([]string, 0, len(nodes)+len(pastBound))
	ownerOf := func(block model.ArtifactBlock) string {
		if _, ok := fragments[block.Document]; ok {
			return block.Document
		}
		return artifactID
	}
	for index, block := range nodes {
		if !fits[index] {
			continue
		}
		ids, documents = append(ids, block.ID), append(documents, ownerOf(block))
	}
	for _, block := range pastBound {
		ids, documents = append(ids, block.ID), append(documents, ownerOf(block))
	}
	// ids is every block the room holds now, past the bound or not: the rows of every other block
	// go (queueRoomBlocks), but those of the blocks the pass retains, a retired document's, which
	// keep the document they name.
	b.room, b.roomIDs, b.roomDocuments = true, ids, documents
	standing := make(map[string]string, len(ids)+len(b.retained))
	for index, id := range ids {
		standing[id] = documents[index]
	}
	for _, id := range b.retained {
		standing[id] = indexed.blocks[id].document
	}
	if len(ids) == 0 {
		return nil, standing, nil
	}
	type move struct{ id, was, now string }
	var moves []move
	if indexed.rooms != nil {
		// The room as the earlier indexing left it: each moved block's two documents, by id, then
		// their ref_keys in one statement.
		var named []string
		for index, id := range ids {
			if was, held := indexed.rooms[id]; held && was != documents[index] {
				moves = append(moves, move{id: id, was: was, now: documents[index]})
				named = append(named, was, documents[index])
			}
		}
		if len(moves) > 0 {
			refKeys := make(map[string]string, len(named))
			rows, err := q.Query(ctx, `select a.id::text, a.ref_key from artifacts a where a.id = any($1::uuid[])`, named)
			if err != nil {
				return nil, nil, fmt.Errorf("read the documents of the room blocks of %s: %w", artifactID, err)
			}
			for rows.Next() {
				var id, refKey string
				if err := rows.Scan(&id, &refKey); err != nil {
					rows.Close()
					return nil, nil, fmt.Errorf("read the documents of the room blocks of %s: %w", artifactID, err)
				}
				refKeys[id] = refKey
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return nil, nil, fmt.Errorf("read the documents of the room blocks of %s: %w", artifactID, err)
			}
			for index := range moves {
				moves[index].was, moves[index].now = refKeys[moves[index].was], refKeys[moves[index].now]
			}
		}
	} else {
		// The statement answers each row whose document the pass moves, with the document the row
		// holds, and the ref_key of each of the two, so the retraction and the assertion need no
		// second read. Each block the room holds probes its own row through issue_room_blocks'
		// key (tree_artifact_id, block_id_key(block_id)), and each moved row its two documents
		// through artifacts' primary key, each probe behind an offset 0 the planner cannot flatten:
		// joined plainly, a plan made on the estimate of a near-empty table read the room's rows
		// once for each block, a cost that grew with the square of the room (37 s at 20,000
		// blocks), where a probe a block costs the same whatever the statistics say
		// (TestThePassLockTimeGrowsLinearlyWithTheRoom).
		rows, err := q.Query(ctx, `
			select wanted.block_id, old.ref_key, new.ref_key
			from unnest($2::text[], $3::text[]) as wanted (block_id, document_id)
			cross join lateral (
				select r.document_id from issue_room_blocks r
				where r.tree_artifact_id = $1::uuid and block_id_key(r.block_id) = block_id_key(wanted.block_id)
				  and r.block_id = wanted.block_id
				offset 0
			) held
			cross join lateral (
				select a.ref_key from artifacts a where a.id = held.document_id offset 0
			) old
			cross join lateral (
				select a.ref_key from artifacts a where a.id = wanted.document_id::uuid offset 0
			) new
			where held.document_id <> wanted.document_id::uuid
		`, artifactID, ids, documents)
		if err != nil {
			return nil, nil, fmt.Errorf("read the documents of the room blocks of %s: %w", artifactID, err)
		}
		for rows.Next() {
			var moved move
			if err := rows.Scan(&moved.id, &moved.was, &moved.now); err != nil {
				rows.Close()
				return nil, nil, fmt.Errorf("read the documents of the room blocks of %s: %w", artifactID, err)
			}
			moves = append(moves, moved)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, nil, fmt.Errorf("read the documents of the room blocks of %s: %w", artifactID, err)
		}
	}
	var tuples []graphstore.UncausedTuple
	for _, moved := range moves {
		if len(moved.id) > text.MaxBlockIDBytes {
			// A block past the bound is no node, and states no in_document.
			continue
		}
		address := blockNodeAddress(document.RefKey, moved.id)
		for _, membership := range []struct {
			to       string
			asserted bool
		}{{moved.was, false}, {moved.now, true}} {
			tuples = append(tuples, graphstore.UncausedTuple{Tuple: graphstore.Tuple{
				FromKind: BlockNodeKind, FromID: address, Kind: TupleInDocument,
				ToKind: graphstore.KindArtifact, ToID: membership.to,
				At: document.At, Who: document.Writer, Asserted: membership.asserted,
			}})
		}
	}
	return tuples, standing, nil
}

// queueRoomBlocks queues the room's record planRoomBlocks planned: the delete of the row of every
// block the room no longer holds, but a block the pass retains (BlockIndexing.retained), then the
// document of each block it holds, written where it moved or is new. A pass over a document that
// is not an issue's tree queues nothing.
func (b BlockIndexing) queueRoomBlocks(batch *store.WriteBatch) {
	if !b.room {
		return
	}
	batch.Queue(fmt.Sprintf("delete the room blocks version of %s no longer holds", b.artifactID), `
		delete from issue_room_blocks r where r.tree_artifact_id = $1::uuid and not (r.block_id = any($2::text[]))
		  and not (r.block_id = any(coalesce($3::text[], '{}')))
	`, b.artifactID, b.roomIDs, b.retained)
	if len(b.roomIDs) == 0 {
		return
	}
	batch.Queue(fmt.Sprintf("record the documents of the room blocks of %s", b.artifactID), `
		insert into issue_room_blocks (tree_artifact_id, block_id, document_id)
		select $1::uuid, w.block_id, w.document_id::uuid from unnest($2::text[], $3::text[]) as w (block_id, document_id)
		on conflict (tree_artifact_id, block_id_key(block_id)) do update set document_id = excluded.document_id
		where issue_room_blocks.document_id is distinct from excluded.document_id
	`, b.artifactID, b.roomIDs, b.roomDocuments)
}

// childRanker ranks the children of one parent at a time, holding the slices that work takes
// from one parent to the next, so a document of many parents allocates them once.
type childRanker struct {
	children, tails, previous []int
	standing                  []string
	kept                      []bool
}

// rank writes into ranks the rank of each of the children, held being the rank each child holds
// now: the longest run of the ranks they hold that stand in the band and already increase in
// document order, compared byte for byte, keeps its ranks (increasingRun), and every other child
// takes rank.Between the kept ranks on either side of it, or the band's own bounds where it has
// none, its first character band and the next. So a block moved under another parent, or among its
// siblings, takes a new rank and its siblings keep theirs; a block that holds a rank of another
// band - one moved into this document from another's fragment - takes one of this band; two
// siblings holding one rank cannot both be kept, so the later is given a new one; and no
// rank.Between is ever asked for bounds out of order. A run of children with no rank to keep, as
// at a document's first index, is spread evenly between its bounds (spreadRanks), so the ranks of
// a list of thousands of items stay a few bytes long. A text_rank is ranked the same way, over the
// blocks that share a text parent (textRankBlocks).
func (r *childRanker) rank(band byte, held func(child int) string, ranks []string) {
	count := len(r.children)
	r.standing = slices.Grow(r.standing[:0], count)[:count]
	for index, child := range r.children {
		r.standing[index] = ""
		if standing := held(child); standsInBand(standing, band) {
			r.standing[index] = standing
		}
	}
	r.increasingRun()
	start, below, above := 0, string(band), string(band+1)
	for index := 0; index <= count; index++ {
		if index < count && !r.kept[index] {
			continue
		}
		bound := above
		if index < count {
			bound = r.standing[index]
		}
		spreadRanks(ranks, r.children[start:index], below, bound)
		if index < count {
			ranks[r.children[index]] = bound
			below = bound
		}
		start = index + 1
	}
}

// increasingRun marks in r.kept the longest run of r.standing, left to right, whose ranks
// increase strictly, an empty rank taking no part, by patience sorting over byte order. Of two
// equal ranks the earlier stays in the run and the later does not, so of two siblings that hold
// one rank the one first in the document keeps it.
func (r *childRanker) increasingRun() {
	count := len(r.standing)
	r.kept = slices.Grow(r.kept[:0], count)[:count]
	clear(r.kept)
	r.tails = r.tails[:0]
	r.previous = slices.Grow(r.previous[:0], count)[:count]
	for index, held := range r.standing {
		if held == "" {
			continue
		}
		at, equal := slices.BinarySearchFunc(r.tails, held, func(tail int, held string) int {
			return strings.Compare(r.standing[tail], held)
		})
		if equal {
			continue
		}
		r.previous[index] = -1
		if at > 0 {
			r.previous[index] = r.tails[at-1]
		}
		if at == len(r.tails) {
			r.tails = append(r.tails, index)
		} else {
			r.tails[at] = index
		}
	}
	if len(r.tails) == 0 {
		return
	}
	for index := r.tails[len(r.tails)-1]; index >= 0; index = r.previous[index] {
		r.kept[index] = true
	}
}

// spreadRanks gives each block of run, in order, a rank strictly between below and above, by
// halving: the middle block takes rank.Between the bounds, and each half is spread between it and
// its own bound. Taking the ranks one after another from below would lengthen them by a byte
// every 32 blocks (rank.Between's after), which a list of thousands of items would carry in every
// rank it holds.
func spreadRanks(ranks []string, run []int, below, above string) {
	if len(run) == 0 {
		return
	}
	middle := len(run) / 2
	ranks[run[middle]] = rank.Between(below, above)
	spreadRanks(ranks, run[:middle], below, ranks[run[middle]])
	spreadRanks(ranks, run[middle+1:], ranks[run[middle]], above)
}

// standsInBand reports whether held is a rank of band that rank.Between may take as a bound: its
// first character the band's and at least one character after it, every character a base-62
// digit, and its last not the alphabet's first, below which no rank sorts and so none can stand
// between it and the rank it ends. A rank no pass of this index wrote - another band's, or
// anything else - is no rank to keep, and its block takes a new one.
func standsInBand(held string, band byte) bool {
	if len(held) < 2 || held[0] != band || held[len(held)-1] == '0' {
		return false
	}
	for index := 1; index < len(held); index++ {
		switch character := held[index]; {
		case character >= '0' && character <= '9', character >= 'A' && character <= 'Z', character >= 'a' && character <= 'z':
		default:
			return false
		}
	}
	return true
}

// planBlockRefs plans the reconciliation of the document's block_refs with the indexing's pairs,
// against the links index holds, and answers what it states in the store, with the links the
// index holds once it is written. It adds to the indexing's moved targets the targets of the
// links it drops and inserts, of those whose block types it changes and of those whose node it
// moves.
//
// Beside them it states what each row it inserts or drops is the projection of: one tuple out of
// the node the row hangs on (blockPair.from) - its block's, or for an ask block's shaped the ask's
// - under that pair's own kind - a pointer attribute's name, or `mentions` for a block's mention -
// asserted for each pair the insert inserts and retracted for each pair the delete drops, so the
// tuple and the row arrive and leave together. A surviving pair earns none while its row names the
// node it hangs on: it keeps its dates and its source event and takes this version's to_types, and
// a tuple appended for it would carry this version's time and win the current read over the one
// that matches the row.
//
// The insert writes a row's node columns where it inserts the row: a pair that names a node
// of its own (blockPair.nodeKind) writes that node into the row, which marks it. A surviving row
// that names no node - a pair a build before the store wrote, or one the release before this one
// inserted after this build's mark was set or after the store last stated the document - takes
// its node, its pair's own or else its block's, in a statement of its own wherever such a row may
// be another release's (BlockIndexing.namesSurvivingRows): on the drain of this build's mark, and
// where the pass reads the store's statement of the document as stale. It takes the one tuple that
// row projects, dated, written and caused as the tuple of a pair the insert inserts. That
// statement names only rows that name no node, so a surviving pair whose row names its node still
// earns no tuple. Any other pass leaves such a row its nulls, which the old block_refs arm
// answers, until the backfill names it and marks the document.
//
// A surviving pair whose row names a node other than the one the pair hangs on now moves to that
// node (rehome): a block retyped between an ask and a decision keeps its id and its tie, and an
// ask block's shaped hangs on the ask's node where a decision's hangs on the block's own. Its
// tuple leaves the node the row names and is stated out of the pair's, and the row is renamed to
// it (queueBlockRefs), so the row and the tuple move together.
//
// A scope (BlockIndexing.scope) limits the rows the reconcile drops to those it reaches: every
// other link the index holds is held as it stands, whatever the pairs hold, so a pass that read
// only its own ties drops nothing it did not read. So is every link of a block the pass retains,
// one of a retired document of the issue room (BlockIndexing.retained).
//
// A pair inserted is dated at the version its block records, and a pair dropped retracted at that
// version's time (BlockIndexing.versionOf): the one version a pass over a document indexes, and
// for an issue room the version of the block's own document. The pairs' far ends are resolved
// before this plan reads them (resolvePointerPairs).
func (b *BlockIndexing) planBlockRefs(ctx context.Context, q Queryer, index BlockIndex, writer model.Actor) ([]graphstore.UncausedTuple, []indexedLink, error) {
	written := make(map[blockPairKey]struct{}, len(b.pairs))
	for _, pair := range b.pairs {
		written[pair.key()] = struct{}{}
	}
	indexed := make(map[blockPairKey]indexedLink, len(index.links))
	var tuples []graphstore.UncausedTuple
	var kept []indexedLink
	for _, link := range index.links {
		indexed[link.key()] = link
		if _, still := written[link.key()]; still {
			continue
		}
		if !b.scope.reaches(link.blockPair) || b.retains(link.blockID) {
			kept = append(kept, link)
			continue
		}
		b.moved = append(b.moved, link.target)
		fromKind, address := link.from(b.refKey)
		// A mention's or a pointer's far end is a reference target, which the block's row holds
		// up to text.MaxTargetBytes but the store keys whole beside the block's own address: a
		// tuple whose far end is past graphstore.MaxAddressBytes was never written, so its
		// retraction is not written either, and the row kept its null node columns (the insert
		// below), which the old block_refs arm answers.
		if !graphstore.AddressFits(address) || !graphstore.AddressFits(link.target.ID) {
			continue
		}
		// The row is gone, so no source_seq is ever written for it and the retraction carries
		// none: its time is the version's of the block (versionOf), the moment the pair stopped
		// being held.
		tuples = append(tuples, graphstore.UncausedTuple{Tuple: graphstore.Tuple{
			FromKind: fromKind, FromID: address, Kind: link.kind,
			ToKind: link.target.Kind, ToID: link.target.ID,
			At: b.versionOf(link.blockID).At, Who: writer, Asserted: false,
		}})
	}
	// On the document's first indexing a new pair dates from the document's own refs row for its
	// target, when one exists, as the insert dates its row: the read is of the rows that insert
	// joins, which no statement of this write changes before it.
	var cited map[Target]citedRef
	if b.firstIndexing && len(b.pairs) > 0 {
		var err error
		if cited, err = documentRefs(ctx, q, b.artifactID); err != nil {
			return nil, nil, err
		}
	}
	links := make([]indexedLink, 0, len(kept)+len(b.pairs))
	links = append(links, kept...)
	for _, pair := range b.pairs {
		link, already := indexed[pair.key()]
		if already {
			var moved []graphstore.UncausedTuple
			link, moved = b.rehome(link, pair, writer)
			tuples = append(tuples, moved...)
		}
		if already && slices.Equal(link.toTypes, pair.toTypes) {
			links = append(links, link)
			continue
		}
		b.moved = append(b.moved, pair.target)
		if already {
			link.toTypes = pair.toTypes
			links = append(links, link)
			continue
		}
		_, address := pair.from(b.refKey)
		fits := graphstore.AddressFits(address) && graphstore.AddressFits(pair.target.ID)
		link = indexedLink{blockPair: pair, createdAt: b.versionOf(pair.blockID).At, named: fits}
		if ref, found := cited[pair.target]; found {
			link.createdAt, link.sourceSeq = ref.createdAt, ref.sourceSeq
		}
		links = append(links, link)
		if fits {
			tuples = append(tuples, assertedLink(b.refKey, link, writer))
		}
	}
	if !b.namesSurvivingRows {
		return tuples, links, nil
	}
	// The pairs whose row survived naming no node. Every block a pair is held by is a block of
	// the version whose node this pass wrote or found named on its document_blocks row, so the
	// foreign key on the columns finds that node, and a pair that names a node of its own names
	// that node, as the insert would; a block or a far end past the address bound is no node of
	// the store, and its row keeps its nulls, as the insert leaves them. Naming the row moves no
	// count: the old arm answered the row, and the tuple arm answers the same link once the row
	// names its node. The links after kept are the pairs', one for each, in their order.
	for position, pair := range b.pairs {
		link := &links[len(kept)+position]
		if _, survives := indexed[link.key()]; !survives || link.named {
			continue
		}
		if _, address := pair.from(b.refKey); !graphstore.AddressFits(address) || !graphstore.AddressFits(link.target.ID) {
			continue
		}
		link.nodeKind, link.nodeAddress, link.named = pair.nodeKind, pair.nodeAddress, true
		tuples = append(tuples, assertedLink(b.refKey, *link, writer))
	}
	return tuples, links, nil
}

// rehome moves link, the row a surviving pair holds, to the node pair hangs on, where that is not
// the node the row names, and answers the link as the index holds it after, with what the move
// states: a retraction of the row's tuple out of the node it named, at the time of the version its
// block records (BlockIndexing.versionOf), as a dropped pair's retraction is dated, and the same
// tuple out of pair's node, dated, written and caused as the tuple of a row the insert inserts
// (assertedLink). The row takes pair's node, or none where that node's address is past the store's
// bound, as the insert leaves such a row (queueBlockRefs), and its target is moved, since the node
// that cites it is another. A row that names no node states no tuple to move: the pass that names
// surviving rows names it (namesSurvivingRows), and every other leaves it to the old block_refs
// arm.
func (b *BlockIndexing) rehome(link indexedLink, pair blockPair, writer model.Actor) (indexedLink, []graphstore.UncausedTuple) {
	if !link.named {
		return link, nil
	}
	heldKind, held := link.from(b.refKey)
	wantedKind, wanted := pair.from(b.refKey)
	if heldKind == wantedKind && held == wanted {
		return link, nil
	}
	stated := []graphstore.UncausedTuple{{Tuple: graphstore.Tuple{
		FromKind: heldKind, FromID: held, Kind: link.kind,
		ToKind: link.target.Kind, ToID: link.target.ID,
		At: b.versionOf(link.blockID).At, Who: writer, Asserted: false,
	}}}
	link.nodeKind, link.nodeAddress = pair.nodeKind, pair.nodeAddress
	link.named = graphstore.AddressFits(wanted)
	if link.named {
		stated = append(stated, assertedLink(b.refKey, link, writer))
	}
	b.rehomed = append(b.rehomed, link)
	b.moved = append(b.moved, link.target)
	return link, stated
}

// citedRef is when the document's own refs row for a target was written and the source event
// it stands at, which a new pair of the document's first indexing takes.
type citedRef struct {
	createdAt time.Time
	sourceSeq *int64
}

// documentRefs reads the document's own refs rows, by target, as the insert of a first
// indexing's pairs joins them.
func documentRefs(ctx context.Context, q Queryer, artifactID string) (map[Target]citedRef, error) {
	rows, err := q.Query(ctx, `
		select to_kind, to_id, created_at, source_seq from refs
		where from_kind = 'artifact' and from_id = $1::uuid::text
	`, artifactID)
	if err != nil {
		return nil, fmt.Errorf("read the references of %s: %w", artifactID, err)
	}
	defer rows.Close()
	cited := map[Target]citedRef{}
	for rows.Next() {
		var target Target
		var ref citedRef
		if err := rows.Scan(&target.Kind, &target.ID, &ref.createdAt, &ref.sourceSeq); err != nil {
			return nil, fmt.Errorf("scan a reference of %s: %w", artifactID, err)
		}
		cited[target] = ref
	}
	return cited, rows.Err()
}

// assertedLink is the tuple a block_refs row the pass writes or names is the projection of: the
// link out of the node the row names (blockPair.from) under the pair's own kind, dated from the
// row's created_at and stated by the version's writer. Its legacy_source_seq is the row's
// source_seq, and a row that holds none takes the cause's (LegacyFromCause): outside a first
// indexing the insert leaves source_seq null and the version's event fills it in this same
// transaction.
func assertedLink(refKey string, link indexedLink, writer model.Actor) graphstore.UncausedTuple {
	fromKind, address := link.from(refKey)
	return graphstore.UncausedTuple{
		Tuple: graphstore.Tuple{
			FromKind: fromKind, FromID: address, Kind: link.kind,
			ToKind: link.target.Kind, ToID: link.target.ID,
			At: link.createdAt, Who: writer, LegacySourceSeq: link.sourceSeq, Asserted: true,
		},
		LegacyFromCause: true,
	}
}

// queueBlockRefs queues the reconciliation of the document's block_refs with the indexing's pairs,
// matching on the five columns of its key (planBlockRefs planned what it states): the delete of
// the pairs the version drops, within the indexing's scope where it has one, the insert of those
// it adds, which names a row's node - its pair's own, or else its block's - only where it inserts
// the row, the move of each surviving row whose pair hangs on another node now (queueRehomed),
// and, where the rows may be another release's (namesSurvivingRows: on the drain of this build's
// mark and where the store's statement is stale), the naming of each surviving row that names
// none. A pair inserted takes the version its block records as its created_version and that
// version's time as its created_at (BlockIndexing.versionOf): the one version a pass over a
// document indexes, and for an issue room the version of the block's own document, so a reader
// keying a pair's version by the block's document (BlockDocumentSQL) finds that document's.
func (b BlockIndexing) queueBlockRefs(batch *store.WriteBatch) {
	pairs := b.pairs
	blockIDs := make([]string, len(pairs))
	kinds := make([]string, len(pairs))
	toKinds := make([]string, len(pairs))
	toIDs := make([]string, len(pairs))
	// A pair's to_types travel as one space-joined text, null for no limit: a block type's name is
	// a directive name (pmdoc), which holds no space.
	toTypes := make([]*string, len(pairs))
	// The node a pair names of its own, null for the block's.
	nodeKinds := make([]*string, len(pairs))
	nodeAddresses := make([]*string, len(pairs))
	createdVersions := make([]int32, len(pairs))
	createdAts := make([]time.Time, len(pairs))
	for i, pair := range pairs {
		blockIDs[i], kinds[i], toKinds[i], toIDs[i] = pair.blockID, pair.kind, pair.target.Kind, pair.target.ID
		if len(pair.toTypes) > 0 {
			joined := strings.Join(pair.toTypes, " ")
			toTypes[i] = &joined
		}
		if pair.nodeKind != "" {
			nodeKinds[i], nodeAddresses[i] = &pair.nodeKind, &pair.nodeAddress
		}
		version := b.versionOf(pair.blockID)
		createdVersions[i], createdAts[i] = int32(version.Number), version.At
	}
	// A scope's kinds and whole blocks are bound as empty lists where it holds none, never as
	// null: a null kinds list is no scope at all, and an empty one reaches no row by that arm.
	var scopeKinds, scopeBlocks []string
	scopeWhole := []string{}
	if b.scope != nil {
		scopeKinds, scopeBlocks = b.scope.kinds, b.scope.blocks
		if scopeKinds == nil {
			scopeKinds = []string{}
		}
		if b.scope.whole != nil {
			scopeWhole = b.scope.whole
		}
	}
	// The rows of a block the pass retains stay as they stand (BlockIndexing.retained).
	batch.Queue(fmt.Sprintf("delete the block mentions version %d of %s dropped", b.version, b.artifactID), `
		delete from block_refs r
		where r.artifact_id = $1 and not exists (
			select 1 from unnest($2::text[], $3::text[], $4::text[], $5::text[]) as p (block_id, kind, to_kind, to_id)
			where p.block_id = r.block_id and p.kind = r.kind and p.to_kind = r.to_kind and p.to_id = r.to_id)
		  and ($6::text[] is null or (r.kind = any($6::text[]) and r.block_id = any($7::text[])) or r.block_id = any($9::text[]))
		  and not (r.block_id = any(coalesce($8::text[], '{}')))
	`, b.artifactID, blockIDs, kinds, toKinds, toIDs, scopeKinds, scopeBlocks, b.retained, scopeWhole)
	if len(pairs) == 0 {
		return
	}
	// A surviving pair takes this version's to_types, so a change of an attribute's targets
	// reaches it here; PlanBlocks named the targets it moves. Adding the node columns to the
	// conflict update would rewrite a surviving row no version changed, so a surviving row that
	// names no node learns its node in the statement after this one, and only where the pass names
	// surviving rows (namesSurvivingRows).
	batch.Queue(fmt.Sprintf("record the block mentions of version %d of %s", b.version, b.artifactID), `
		insert into block_refs (artifact_id, block_id, from_node, node_kind, node_address, to_kind, to_id, kind, to_types, created_version, created_at, source_seq)
		select $1::uuid, p.block_id, a.ref_key || '#' || p.block_id,
		       case when octet_length(p.to_id) > $11 then null
		            when p.node_kind is not null then p.node_kind
		            when octet_length(a.ref_key || '#' || p.block_id) <= $11 then $10 end,
		       case when octet_length(p.to_id) > $11 then null
		            when p.node_kind is not null then p.node_address
		            when octet_length(a.ref_key || '#' || p.block_id) <= $11 then a.ref_key || '#' || p.block_id end,
		       p.to_kind, p.to_id, p.kind, string_to_array(p.to_types, ' '), p.created_version,
		       coalesce(case when $8 then r.created_at end, p.created_at), case when $8 then r.source_seq end
		from unnest($2::text[], $3::text[], $4::text[], $5::text[], $9::text[], $12::text[], $13::text[], $6::int[], $7::timestamptz[])
		     as p (block_id, kind, to_kind, to_id, to_types, node_kind, node_address, created_version, created_at)
		cross join (select ref_key from artifacts where id = $1::uuid) a
		left join refs r on r.from_kind = 'artifact' and r.from_id = $1::uuid::text
		                and r.to_kind = p.to_kind and r.to_id = p.to_id
		on conflict (artifact_id, block_id, to_kind, to_id, kind) do update set to_types = excluded.to_types
		where block_refs.to_types is distinct from excluded.to_types
	`, b.artifactID, blockIDs, kinds, toKinds, toIDs, createdVersions, createdAts, b.firstIndexing, toTypes, BlockNodeKind, MaxBlockNodeBytes,
		nodeKinds, nodeAddresses)
	b.queueRehomed(batch)
	if !b.namesSurvivingRows {
		return
	}
	batch.Queue(fmt.Sprintf("name the nodes of the block mentions of %s that name none at version %d", b.artifactID, b.version), `
		update block_refs r
		set node_kind = coalesce(p.node_kind, $6), node_address = coalesce(p.node_address, $7 || '#' || r.block_id)
		from unnest($2::text[], $3::text[], $4::text[], $5::text[], $9::text[], $10::text[])
		     as p (block_id, kind, to_kind, to_id, node_kind, node_address)
		where r.artifact_id = $1 and r.block_id = p.block_id and r.kind = p.kind
		  and r.to_kind = p.to_kind and r.to_id = p.to_id and r.node_kind is null
		  and (p.node_kind is not null or octet_length($7 || '#' || r.block_id) <= $8)
		  and octet_length(r.to_id) <= $8
	`, b.artifactID, blockIDs, kinds, toKinds, toIDs, BlockNodeKind, b.refKey, MaxBlockNodeBytes, nodeKinds, nodeAddresses)
}

// queueRehomed queues the move of each surviving row whose pair hangs on another node now
// (rehome) to that node, or to none where that node's address is past the store's bound, matching
// on the five columns of the row's key.
func (b BlockIndexing) queueRehomed(batch *store.WriteBatch) {
	if len(b.rehomed) == 0 {
		return
	}
	blockIDs := make([]string, len(b.rehomed))
	kinds := make([]string, len(b.rehomed))
	toKinds := make([]string, len(b.rehomed))
	toIDs := make([]string, len(b.rehomed))
	nodeKinds := make([]*string, len(b.rehomed))
	nodeAddresses := make([]*string, len(b.rehomed))
	for i, link := range b.rehomed {
		blockIDs[i], kinds[i], toKinds[i], toIDs[i] = link.blockID, link.kind, link.target.Kind, link.target.ID
		if link.named {
			kind, address := link.from(b.refKey)
			nodeKinds[i], nodeAddresses[i] = &kind, &address
		}
	}
	batch.Queue(fmt.Sprintf("move the block mentions of version %d of %s to the node each hangs on now", b.version, b.artifactID), `
		update block_refs r
		set node_kind = p.node_kind, node_address = p.node_address
		from unnest($2::text[], $3::text[], $4::text[], $5::text[], $6::text[], $7::text[])
		     as p (block_id, kind, to_kind, to_id, node_kind, node_address)
		where r.artifact_id = $1 and r.block_id = p.block_id and r.kind = p.kind
		  and r.to_kind = p.to_kind and r.to_id = p.to_id
	`, b.artifactID, blockIDs, kinds, toKinds, toIDs, nodeKinds, nodeAddresses)
}

// PlaceBlocks is where the block index places each block of a version of the document refKey
// names. It answers the version's unique blocks (the first block of each id), the index among
// them of each block it states a part_of for, in document order, and beside each of those the
// index among them of the block that holds it, -1 for a block the document holds. blocks are
// the version's rendered blocks, as PlanBlocks takes them.
//
// It is the one walk the index states each part_of from (planBlockParents) and the sizing
// counts the parents a version moved with (graphcensus), so what the sizing counts and what the
// writer appends cannot drift.
func PlaceBlocks(refKey string, blocks []model.ArtifactBlock) (nodes []model.ArtifactBlock, placed, parents []int) {
	nodes, _, _ = uniqueBlocks(blocks)
	fits, _ := blockFits(refKey, nodes)
	placed, parents = placeBlocks(nodes, fits)
	return nodes, placed, parents
}

// placeBlocks is PlaceBlocks over a version's unique blocks and whether each one's address fits
// the store's key. A block whose address is no key of the store is no node of it: nothing is
// stated about the block itself, and a block it holds is placed in the nearest block above it
// that is a node, or in the document.
func placeBlocks(nodes []model.ArtifactBlock, fits []bool) (placed, parents []int) {
	held := blockParents(nodes)
	placed = make([]int, 0, len(nodes))
	parents = make([]int, 0, len(nodes))
	for index := range nodes {
		if !fits[index] {
			continue
		}
		parent := -1
		for above := held[index]; above >= 0; above = held[above] {
			if fits[above] {
				parent = above
				break
			}
		}
		placed = append(placed, index)
		parents = append(parents, parent)
	}
	return placed, parents
}

// parentTuple is one statement about the block that holds another: an assertion of the parent
// the render gives, or a retraction of the one the store still asserts.
func parentTuple(address, toKind, toID string, at time.Time, writer model.Actor, asserted bool) graphstore.UncausedTuple {
	return graphstore.UncausedTuple{Tuple: graphstore.Tuple{
		FromKind: BlockNodeKind, FromID: address, Kind: graphstore.TuplePartOf,
		ToKind: toKind, ToID: toID,
		At: at, Who: writer, Asserted: asserted,
	}}
}

// blockParents names, for each block, the index of the tightest block of the same document
// that holds it, or -1 for a block at the document's top level. blocks are in document order,
// each parent before its children, which is the order innermostBlocks walks too; over a
// block's own From that helper answers the block itself, because it pushes every block
// starting at or before the offset before it reads the top of its stack, so the parent is the
// entry below the block on the stack and not the top of it.
func blockParents(blocks []model.ArtifactBlock) []int {
	parents := make([]int, len(blocks))
	stack := make([]int, 0, 8)
	for index, block := range blocks {
		for len(stack) > 0 && blocks[stack[len(stack)-1]].To <= block.From {
			stack = stack[:len(stack)-1]
		}
		parents[index] = -1
		if len(stack) > 0 {
			parents[index] = stack[len(stack)-1]
		}
		stack = append(stack, index)
	}
	return parents
}
