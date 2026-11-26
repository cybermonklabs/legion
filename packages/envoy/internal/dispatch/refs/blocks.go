package refs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/itemblocks"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// Pointer is one reference a typed block's `block[]` attribute holds (pmdoc.BlockPointer): the
// typed block's id and type, the link's kind (the attribute's name), the block reference it names,
// and the block types the attribute points at (none: any block).
type Pointer struct {
	Block, BlockType, Kind string
	Target                 text.Ref
	Targets                []string
}

// PointersOf is each `block[]` value the typed blocks of tree hold (pmdoc.BlockPointers), as the
// block index takes them.
func PointersOf(tree *pmdoc.Node) []Pointer {
	found := pmdoc.BlockPointers(tree)
	pointers := make([]Pointer, len(found))
	for index, pointer := range found {
		pointers[index] = Pointer{Block: pointer.Block, BlockType: pointer.BlockType, Kind: pointer.Attribute, Target: pointer.Target, Targets: pointer.Targets}
	}
	return pointers
}

// BlockNodeKind is what the node and tuple store calls one block of a document, and
// BlockSupertag the supertag the node of a block of no typed kind carries: a paragraph, a heading,
// a list, a code block, and a reading in an ask nobody has answered. The node of a typed block
// carries its type's supertag instead (TypeSupertag, R26 of the issue-tree spec), and
// its row's `type` column projects it. Every read that finds block nodes selects them by their
// kind, never by the supertag BlockSupertag.
const (
	BlockNodeKind = "block"
	BlockSupertag = "block"
)

// TupleInDocument carries a block's membership of its document: the fact `graph_edges` reads
// off a document_blocks row, dated that row's own created_at. Beside it graphstore.TuplePartOf
// carries the block of the same document that holds this one, which document_blocks's
// parent_block_id projects and which is the document's own artifact address for a block no
// block holds.
const TupleInDocument = "in_document"

// BlockRefKindsSQL is a select of every tuple kind the block index writes into block_refs, one
// row each, so of every kind of tuple out of a block that a block_refs row is the projection of: a
// block's mention (blockPairs) and the name of every `block[]` attribute the store's type nodes
// declare, the kind of each pointer that attribute makes (pointerPairs: a pointer's kind is its
// attribute's name as a document writes it, pmdoc.BlockPointer's Attribute, and never its
// store_name). It reads the store's attribute nodes rather than the schema a process installed,
// so it answers the same for every build reading one store: a build older than the store installs
// no attribute whose `since` is above it (pmdoc.Install), and an attribute node stated gone is
// installed by none, while the block_refs rows of its kind stand until a version re-indexes their
// documents. So an attribute node is read at any state. A tuple of any other kind out of a block -
// its placement, in_document and part_of, or one another writer states of it - is no part of that
// projection, and the store census holds the block_refs rows to the tuples of these kinds alone
// (graphcensus).
const BlockRefKindsSQL = `select 'mentions'
		union select attribute.attributes->>'name' from graph_nodes attribute
		where attribute.kind = 'attribute' and attribute.attributes->>'kind' = '` + string(pmdoc.BlockAttributeBlockList) + `'`

// Version is one version of a document a block index pass covers: its number and the created_at
// of its row.
type Version struct {
	Number int
	At     time.Time
}

// Versions is the version of each document one block index pass covers, by the document's
// artifact id (PlanBlocks). A pass over a document that is its own room covers one version of
// that document (OneVersion). A pass over an issue room, which indexes the room's blocks under its
// tree artifact, covers a version of each document whose fragment of the room holds its blocks -
// the version the room's settlement wrote of that document, or the newest it holds - and one of
// the tree artifact itself for the blocks of its own fragment: a block of the room records a
// version of its own document, never a number of the tree artifact's history, so two documents of
// one room can each hold a block at the same number.
type Versions map[string]Version

// OneVersion is the Versions of a pass over version number of document artifactID alone, written
// at at.
func OneVersion(artifactID string, number int, at time.Time) Versions {
	return Versions{artifactID: {Number: number, At: at}}
}

// Newest is the document and the version of v written last: the version a pass over v states the
// facts that name no document of their own in the name of and at the time of (PlanBlocks), and
// whose first author is the pass's writer (ReadIndexedVersion's writtenIn). Of versions written at
// one time, artifactID's own comes first, then the document of the lowest id, so the choice does
// not depend on the map's order. An empty v answers artifactID at no version.
func (v Versions) Newest(artifactID string) (string, Version) {
	before := func(document, lead string) bool {
		return document == artifactID || lead != artifactID && document < lead
	}
	lead, version, found := artifactID, Version{}, false
	for document, candidate := range v {
		if !found || candidate.At.After(version.At) || candidate.At.Equal(version.At) && before(document, lead) {
			lead, version, found = document, candidate, true
		}
	}
	return lead, version
}

// of is the document whose version a block of document records in a pass over v, and that
// version: document's own where v covers it, else artifactID's where v covers that, else the
// newest v covers (Newest). A block of no other document than artifactID passes document empty
// or artifactID itself.
func (v Versions) of(document, artifactID string) (string, Version) {
	if version, covered := v[document]; covered {
		return document, version
	}
	if version, covered := v[artifactID]; covered {
		return artifactID, version
	}
	return v.Newest(artifactID)
}

// BlockRefKinds reads the kinds BlockRefKindsSQL selects through q. A read whose plan may reach
// graph_nodes only by its key reads them here first, by a statement of its own in the caller's
// transaction, and binds them as a text[] parameter (`t.kind = any($N::text[])`), so the read of
// the store's attribute nodes is no part of its plan: the store's held statement of a document
// (heldDocumentBlocks). An attribute node that names no attribute is no kind.
func BlockRefKinds(ctx context.Context, q Queryer) ([]string, error) {
	var kinds []string
	if err := q.QueryRow(ctx, `select array_agg(linked.kind) from (`+BlockRefKindsSQL+`) linked (kind)
		where linked.kind is not null`).Scan(&kinds); err != nil {
		return nil, fmt.Errorf("read the kinds block_refs projects: %w", err)
	}
	return kinds, nil
}

// BlockIndex is what a document's block index holds: each block's row - the sha256 of its text,
// when the row was written, whether it names the block's node and the document it is a block of -
// by block id; the links its blocks hold, each with the block types it points at, when its row was
// written, the source event it stands at and whether it names its block's node; and the version
// the document's blocks stand at (blockStand), which the revision fence is decided on. respellable
// is, by block id, the version a block's text last changed in, for each block whose changed
// version another renderer version stored or none stamped (pmdoc.OtherRenderer), which a version
// the document no longer has counts as: the next version may hold such a block's tree in this
// renderer version's spelling (BlockIndexing.KeepRespelled).
//
// The rest is, for the Held of an indexing the transaction has queued and not written, what that
// indexing left that the store holds only once the write phase has run, each nil for an index read
// from the tables, whose facts the plan reads from the store: placed, the far end it gave each
// block's part_of, by block id; stands, the stand of each other document of an issue room it
// covered; values, the rank, text, placement, text_parent, text_rank and size it stated of each
// block it placed; rooms, the document an issue room's record names for each block; authored, the
// blocks whose authors it stated or found stated (blockAuthors), so a later indexing states none of
// them twice.
//
// stored is, by node address, each block a move of the node route placed in the transaction that
// plans the indexing (StoredBlock, WithStored): the move's own part_of, rank and placement are
// queued in that transaction's write phase, so no read of the plan sees them, and the plan reads
// them from here instead. It is nil for every index no such move holds.
type BlockIndex struct {
	blocks      map[string]storedBlock
	respellable map[string]int
	links       []indexedLink
	stand       blockStand
	placed      map[string]blockFarEnd
	stands      map[string]blockStand
	values      map[string]heldBlockValue
	rooms       map[string]string
	authored    map[string]struct{}
	stored      map[string]StoredBlock
}

// StoredBlock is where a move of the node route (api's PATCH /api/v1/issues/{key}/nodes/{id})
// places a block it moves, in the transaction that also writes a version of the block's document:
// the far end of the part_of the move appends, and the rank it gives the block there, "" for a
// block that holds none. The move states the block's placement PlacementStored beside them, and
// rewrites the parent attribute of a typed block's text to match the new parent in the same
// transaction, so an index pass of that transaction keeps the block where the move put it: its
// part_of, its rank and its placement (placeAuthored, planBlockParents, planBlockValues), and
// states its text, its text_parent, its text_rank and its links from the text as it does for every
// block.
type StoredBlock struct {
	ParentKind, ParentID string
	Rank                 string
}

// WithStored is i holding stored, by node address, as the blocks a move of the reading transaction
// placed (StoredBlock), which every plan of the index reads in place of what the store holds of
// them. A nil stored holds none.
func (i BlockIndex) WithStored(stored map[string]StoredBlock) BlockIndex {
	i.stored = stored
	return i
}

// storedMove is the move of the reading transaction that placed the block at address, and false
// for a block no such move placed.
func (i BlockIndex) storedMove(address string) (StoredBlock, bool) {
	move, moved := i.stored[address]
	return move, moved
}

// indexedLink is one block_refs row the index holds: its pair, with the node it hangs on where
// that is not its block's (blockPair.nodeKind), when the row was written, the source event it
// stands at, and whether it names its node.
type indexedLink struct {
	blockPair
	createdAt time.Time
	sourceSeq *int64
	named     bool
}

// ReadBlockIndex reads what q's block index holds for artifactID: its blocks, its links read with
// the predicate the reconciling delete visits them by, each with the block types it points at, so
// a plan can tell a link whose attribute now names other types, and the version its blocks stand
// at, all in the read phase of the write that plans the indexing (PlanBlocks).
func ReadBlockIndex(ctx context.Context, q Queryer, artifactID string) (BlockIndex, error) {
	blocks, respellable, err := storedBlocks(ctx, q, artifactID)
	if err != nil {
		return BlockIndex{}, err
	}
	rows, err := q.Query(ctx, `
		select block_id, kind, to_kind, to_id, to_types, created_at, source_seq, node_kind, node_address
		from block_refs where artifact_id = $1
	`, artifactID)
	if err != nil {
		return BlockIndex{}, fmt.Errorf("read the block mentions of %s: %w", artifactID, err)
	}
	defer rows.Close()
	var links []indexedLink
	for rows.Next() {
		var link indexedLink
		var nodeKind, nodeAddress *string
		if err := rows.Scan(&link.blockID, &link.kind, &link.target.Kind, &link.target.ID, &link.toTypes,
			&link.createdAt, &link.sourceSeq, &nodeKind, &nodeAddress); err != nil {
			return BlockIndex{}, fmt.Errorf("scan a block mention of %s: %w", artifactID, err)
		}
		link.named = nodeKind != nil && nodeAddress != nil
		// A row marked with a node other than its block's - an ask block's shaped, on the ask's
		// node - is stated out of that node, as its retraction is once the row goes.
		if link.named && *nodeKind != BlockNodeKind {
			link.nodeKind, link.nodeAddress = *nodeKind, *nodeAddress
		}
		links = append(links, link)
	}
	if err := rows.Err(); err != nil {
		return BlockIndex{}, fmt.Errorf("read the block mentions of %s: %w", artifactID, err)
	}
	stand, err := readBlockStand(ctx, q, artifactID)
	if err != nil {
		return BlockIndex{}, err
	}
	return BlockIndex{blocks: blocks, respellable: respellable, links: links, stand: stand}, nil
}

// indexedBefore is the latest version a pass indexed before version, 0 for none
// (document_indexed_versions): the versions between it and version are the ones this pass covers
// that no pass indexed (textSince). The stand's newest indexed version answers it where that one
// is older than version, as it is on the request path, where every version is indexed by its own
// write, and for an indexing planned on the Held of an earlier one in the same transaction; a pass
// at a version some pass already indexed, or below it, reads it.
func indexedBefore(ctx context.Context, q Queryer, artifactID string, version int, stand blockStand) (int, error) {
	if stand.latestIndexed == nil {
		return 0, nil
	}
	if *stand.latestIndexed < version {
		return *stand.latestIndexed, nil
	}
	var before int
	if err := q.QueryRow(ctx, `
		select coalesce(max(version), 0) from document_indexed_versions
		where artifact_id = $1 and version < $2
	`, artifactID, version).Scan(&before); err != nil {
		return 0, fmt.Errorf("read the latest version of %s indexed before %d: %w", artifactID, version, err)
	}
	return before, nil
}

// textSinceIn is textSince over blocks of several documents: ids are the blocks whose text the
// pass records, beside bodies, each read against the versions of its own document (documentOf)
// that the pass covers and no pass indexed before (indexedBefore), through q in the plan's read
// phase. A block of a document versions covers no version of takes the version versionIn answers
// for it, whose history it does not read.
func textSinceIn(
	ctx context.Context,
	q Queryer,
	versions Versions,
	indexedBefore map[string]int,
	ids []string,
	bodies []*string,
	documentOf func(string) string,
	versionIn func(string) Version,
) ([]int32, []time.Time, error) {
	since := make([]int32, len(ids))
	at := make([]time.Time, len(ids))
	grouped := map[string][]int{}
	for index, id := range ids {
		document := documentOf(id)
		grouped[document] = append(grouped[document], index)
	}
	for _, document := range slices.Sorted(maps.Keys(grouped)) {
		indexes := grouped[document]
		version, covered := versions[document]
		if !covered {
			version = versionIn(document)
			for _, index := range indexes {
				since[index], at[index] = int32(version.Number), version.At
			}
			continue
		}
		held := make([]*string, len(indexes))
		for position, index := range indexes {
			held[position] = bodies[index]
		}
		documentSince, documentAt, err := textSince(ctx, q, document, version.Number, version.At, indexedBefore[document], held)
		if err != nil {
			return nil, nil, err
		}
		for position, index := range indexes {
			since[index], at[index] = documentSince[position], documentAt[position]
		}
	}
	return since, at, nil
}

// textSince is, for each block whose text this pass records (bodies, its text in this version),
// the version that text has stood since and that version's time, which the block's row takes as
// its changed_version and changed_at. It is this version where the pass covers no other: on the
// request path every version is indexed by its own write, and indexedBefore is the one before it.
// A pass can cover more than one - a document past the deferral bound, or versions a build
// before wrote during the roll, each of which the block-index reconciliation indexes at the latest
// owed version alone (docs' indexOwedDocument) - and then the text stands since the earliest of
// the owed versions from which every version up to this one holds a block of that same text, as
// the renderer cuts the version's stored markdown (versionBlockTexts). So a version that rewrote
// a block and a later one that left it alone, indexed in one pass, date the change to the first,
// and the text-change time a reader takes from the row (c-spec-tie) is that
// version's. The owed versions are read newest first and only while some block still matches, and
// not at all on the request path. The read is the plan's, in its write's read phase.
func textSince(ctx context.Context, q Queryer, artifactID string, version int, versionAt time.Time, indexedBefore int, bodies []*string) ([]int32, []time.Time, error) {
	since := make([]int32, len(bodies))
	at := make([]time.Time, len(bodies))
	pending := make([]int, len(bodies))
	for index := range bodies {
		since[index], at[index], pending[index] = int32(version), versionAt, index
	}
	if indexedBefore >= version-1 {
		return since, at, nil
	}
	rows, err := q.Query(ctx, `
		select number, markdown, created_at from artifact_versions
		where artifact_id = $1::uuid and number > $2 and number < $3 and markdown is not null
		order by number desc
	`, artifactID, indexedBefore, version)
	if err != nil {
		return nil, nil, fmt.Errorf("read the versions of %s no pass indexed before %d: %w", artifactID, version, err)
	}
	defer rows.Close()
	for len(pending) > 0 && rows.Next() {
		var number int32
		var markdown string
		var created time.Time
		if err := rows.Scan(&number, &markdown, &created); err != nil {
			return nil, nil, fmt.Errorf("read a version of %s no pass indexed: %w", artifactID, err)
		}
		held := versionBlockTexts(markdown)
		standing := pending[:0]
		for _, index := range pending {
			if _, holds := held[*bodies[index]]; holds {
				since[index], at[index] = number, created
				standing = append(standing, index)
			}
		}
		pending = standing
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("read the versions of %s no pass indexed before %d: %w", artifactID, version, err)
	}
	return since, at, nil
}

// versionBlockTexts is the text of every block of a version's stored markdown, as the renderer
// cuts it for the block index: the markdown read back (pmdoc.ParseRendering) and rendered with its
// block offsets. A version that does not read back holds no block textSince can match.
func versionBlockTexts(markdown string) map[string]struct{} {
	tree, err := pmdoc.ParseRendering(markdown)
	if err != nil {
		return nil
	}
	rendered, offsets, err := pmdoc.RenderWithBlockOffsets(tree)
	if err != nil {
		return nil
	}
	texts := make(map[string]struct{}, len(offsets))
	for _, offset := range offsets {
		texts[rendered[offset.From:offset.To]] = struct{}{}
	}
	return texts
}

// IndexedVersion is what a version's own row and its document's row say of its block index: the
// document's ref_key, which every block's address starts with; the authors the version's row
// records, in its own order, and the first of them, its writer, which every node and tuple the
// index writes carries; and the project the document belongs to, or for an agent conversation's
// file the session it is between, which every block node the index writes carries
// (graphstore.Node's Project and Parties). A conversation holds no markdown document (api's
// agentOwnsFilesOnly), so no block has parties today; the record keeps the one rule every
// document's node follows. Issue is the issue that holds the document, "" for one a project holds,
// which a decision's or an ask's tie to another issue is read against (DecisionFacts), and an
// authored node's place (placeAuthored). A version no row records - a caller indexing blocks of
// its own, as the seed does - leaves no author and the writer nobody, the zero actor, exactly as a
// node of the store written on a credential that names nobody carries. A writer whose version row
// is only queued hands it over from what that row will hold (docs' planBlockIndex), since no read
// of the row answers yet; one whose row is stored reads it (ReadIndexedVersion).
//
// Band is the first byte of every rank a block of the document stands at (DocumentBandSQL, or
// DocumentBand for a row only queued), and At is the pass's own time: every placement and every
// value the pass states is dated there. It is the database's clock_timestamp(), read once the
// pass holds the document's owner lock (ReadIndexedVersion reads it, and PlanBlocks reads it for
// a record that carries none), and not the transaction's now(): a pass that waited at the owner
// lock behind another pass of the same document began before that one committed and appends its
// tuples after it, so only a time read once the wait is over orders the tuples' times as their
// seqs are ordered. It is later than the version the pass describes, on the request path as on the
// block-index reconciliation's, so no tuple a renderer reads claims to describe a moment before
// the pass that wrote it.
type IndexedVersion struct {
	RefKey  string
	Writer  model.Actor
	Authors []model.Actor
	Project string
	Parties []string
	Issue   string
	Band    byte
	At      time.Time
}

// VersionBlocks is the version a block-index pass indexes (PlanBlocks), the inputs that travel
// together from the writer that wrote or read the version into the plan: the document
// (ArtifactID), what the newest version's own row says of its index (Record), the version of each
// document the pass covers (Versions: the one version of a pass over a document, OneVersion, or
// the version of each document whose fragment of an issue room holds its blocks), its markdown,
// its rendered blocks (Blocks), the Mentions of its text (Located), its `block[]` values
// (Pointers), and the deployment its text was read against (ServerURL), whose dashboard URLs are
// references and which each block's excerpt is cut around (BlockExcerpt); what the typed-node pass
// read of the version (Typed, DecisionFacts), and what the caller states beside the pass under the
// same event (Beside).
type VersionBlocks struct {
	ArtifactID string
	Record     IndexedVersion
	Versions   Versions
	Markdown   string
	Blocks     []model.ArtifactBlock
	Located    []text.Located
	Pointers   []Pointer
	ServerURL  string
	Typed      TypedFacts
	Beside     graphstore.Uncaused
}

// BlockIndexing is one version's block index, planned against what the index holds
// (PlanBlocks), so the targets it moves and the facts it states in the node and tuple store are
// known before anything is written, and written by Queue, which puts its statements on a write
// phase's batch and does nothing else. An indexing that lost the revision fence (Won) writes
// nothing at all - no projection row and no tuple - since what it would state is what the winning
// version already stated.
type BlockIndexing struct {
	artifactID string
	refKey     string
	// version and versionAt are the newest version the pass covers and its time (Versions.Newest):
	// what a fact that names no block of its own is dated at. versions is the version of each
	// document the pass covers, and documentOf the document each block of the pass is a block of,
	// by id, where that is another than artifactID: the version each block's rows and facts carry
	// (versionOf).
	version       int
	versionAt     time.Time
	versions      Versions
	documentOf    map[string]string
	firstIndexing bool
	won           bool
	// rule is the marking rule of the build planning the pass (BlockIndexMarkingRule), which the
	// fence decides the pass's win at the version the row stands at on, and whether its stand
	// clears the document's mark (queueStand).
	rule int
	// marked says the pass drains this build's mark (blockStand.owed) of artifactID or of a
	// document of the issue room it covers (Marked).
	marked bool
	// namesSurvivingRows says the pass names the node of every surviving block_refs row of the
	// document that names none, stating the link that row projects (queueBlockRefs), because
	// such a row may be another release's: on the drain of this build's mark (marked) and where
	// the pass reads the store's statement of the document as stale (blockStand.stale).
	namesSurvivingRows bool
	// deleted are the blocks the version no longer holds; ids, types, sums and excerpts the
	// blocks whose text it changed (new, changed or restored), and since and sinceAt the version
	// each one's text stands since and that version's time (textSince); textIDs and texts the
	// block texts it records, nil for a deleted block, and textVersions the version each is
	// recorded at.
	deleted, ids, types, sums, excerpts, textIDs []string
	texts                                        []*string
	since, textVersions                          []int32
	sinceAt                                      []time.Time
	// candidates are the blocks among ids whose changed version another renderer version stored
	// or none stamped and that aloneCandidates keeps, which KeepRespelled reads and decides;
	// respelledIDs, respelledSums and respelledExcerpts are those it keeps the changed version of,
	// with their text's sha256 and excerpt at this version, which Queue writes.
	candidates                                     []respellCandidate
	respelledIDs, respelledSums, respelledExcerpts []string
	pairs                                          []blockPair
	// scope, where it is not nil, is the rows of block_refs the reconcile may delete: those of
	// its kinds held by its blocks, the typed-node pass's own ties (PlanDecisionNodes,
	// PlanOpenedAskTies). Every other row of the document is left as it stands.
	scope *pairScope
	// placedIDs and placedParents are the blocks the pass places, in document order, and the
	// block each sits in, nil for one the document holds: what the parents' projection writes.
	placedIDs     []string
	placedParents []*string
	// room says the pass indexes an issue room under its tree artifact, whose record of each
	// block's document it rewrites (issue_room_blocks): roomIDs are the blocks the room holds, and
	// roomDocuments the document of each (planRoomBlocks, queueRoomBlocks).
	room                   bool
	roomIDs, roomDocuments []string
	// moved is what Moved answers: first the targets of the links the version inserts, deletes,
	// retypes or rehomes, linkMoves of them, then those of the links in a block whose text it
	// changed.
	moved     []Target
	linkMoves int
	// rehomed is each surviving link whose node the pass moves (planBlockRefs): a block retyped
	// between an ask and a decision holds the same pair, which hangs on another node now. Each
	// carries the node it hangs on after the move, which queueBlockRefs writes into its row.
	rehomed []indexedLink
	held    BlockIndex
	// stated is what the pass states in the node and tuple store, held for the event that causes
	// the write (graphstore.Uncaused).
	stated graphstore.Uncaused
	// retained are the blocks of a retired document of the issue room the pass indexes that no
	// fragment of the room holds now, in id order (storedBlock.retired): a retired document's
	// fragment leaves its room's layout (docs' roomLayoutSQL), so no version of the room renders
	// its blocks again, and the pass keeps each one's row, text, links, room record and node as the
	// retirement left them rather than dropping them (docs.RetireDocument keeps them).
	retained []string
	// outside are the blocks of the indexed issue room whose rows name a document the pass covers
	// no version of: a room left owed is written one fragment at a time (DecisionIndexing's
	// PlanAuthored), and an authored node of that fragment may take a block of another fragment,
	// which stands in the store, as its parent (placeAuthored). Nil for a pass over every fragment.
	outside map[string]struct{}
}

// versionOf is the version block blockID records: that of the document whose fragment of an
// issue room holds it (documentOf), where the pass covers that document, else the indexed
// document's own, else the newest the pass covers (Versions.of).
func (b BlockIndexing) versionOf(blockID string) Version {
	_, version := b.versions.of(b.documentOf[blockID], b.artifactID)
	return version
}

// retains says the pass keeps block blockID as the retirement of its document left it (retained).
func (b BlockIndexing) retains(blockID string) bool {
	_, found := slices.BinarySearch(b.retained, blockID)
	return found
}

// PlanBlocks plans the block index of version (VersionBlocks) against index, what the document's
// block index holds: one ReadBlockIndex read, or the Held of the caller's own earlier indexing of
// the document that is not written yet. version.Versions is the one version a pass over a
// document indexes (OneVersion), or, for an issue room indexed under its tree artifact
// version.ArtifactID, the version of each document whose fragment holds its blocks (Versions):
// each block records the version of its own document, the document model.ArtifactBlock's
// Document names, or the tree artifact's own for a block of no other. version.Record is what the
// newest of those versions' row and the document's row say of the pass (ReadIndexedVersion over
// Versions.Newest). rule is the marking rule of the build planning it (BlockIndexMarkingRule, or
// the rule a docs.Service carries), which the fence reads (queueStand). It does the indexing's
// whole work - each block's text sum, its excerpt, the links its mentions and its pointers write,
// and what the node and tuple store is told of them - and reads, through q, only what the store
// itself holds where the plan compares the render against it: each block's parent, values and
// authors, where its authored nodes stand, the document's own references on its first indexing,
// the store's statement of the document where that is stale, and the pointer tuples at each block
// it states gone (retractGonePointers). A version writer plans the indexing in its read phase and
// queues it in its write phase (docs.indexDocument). now is the transaction's now(), which a row
// the indexing inserts takes from its column's default.
//
// The plan makes the version's blocks graph nodes, and each mention written inside a block an edge
// out of the innermost block holding its start. version.Blocks are its rendered blocks in
// document order, each parent before its children, and the first holder of a repeated id is the
// node; version.Located must be the Mentions of the document's text (NewReader), the one reading of it a
// version's index takes, so a mention's offset places it in the block holding it however the text
// wrote the reference. A block whose id is longer than text.MaxBlockIDBytes, which no key of the
// index can hold, is no node: a mention inside it starts at the innermost node holding it, if
// any, and the plan logs a warning naming the document, the longest such id's length and the
// bound.
//
// A block whose text (markdown[From:To]) is unchanged keeps the version its text last changed in;
// a new, changed or restored block takes the version its text stands since (textSince) - this
// version, or where the pass covers versions no pass indexed, the earliest of them from which its
// text stood - dated that version's time, and its text is recorded in document_block_texts at that
// version; its excerpt is its text as BlockExcerpt cuts it, the excerpt a document's mention has
// shown since migration 0092, around the dashboard URLs of version.ServerURL. A block of an issue
// room cut from one fragment and pasted with the same text into another moved into another
// document, whose version it records from here on: its row and its text are written again, as a
// changed block's are. A block the version no longer holds is deleted, recorded there with no
// text, its node's state stated gone, and the edges at it stop resolving until a version holds its
// id again. Such a block also takes its pointer tuples with it: each prevented_by of a risk it
// covered as a check, and each constrained_by it held as a requirement, is retracted beside the
// facts the pass states (retractGonePointers), and a version that restores the block restores none
// of them. A block the version holds keeps the state and the visibility its node has, unless it
// was gone, when it comes back live (blockNode restates every node it writes). The windows the
// version's blocks are embedded in are not planned here and not written by this index: the row it
// records marks the document owed them, and the block-index tick's window arm writes them (Queue,
// DocumentsOwedWindows).
//
// A block whose row the pass writes first is made by the first author of the version that first
// holds it, its text's since (textSince), at that version's time. Every block of the version whose
// node records its authors - an authored node anywhere, and every block of an issue's tree
// artifact but a decision and a reading (recordsAuthors) - and states none yet takes one
// authored_by per author of the version that first holds it, in that version's own order, and the
// authors' nodes with them (blockAuthors): a block this pass writes first, and one a build before
// this one wrote, which takes them at the pass that next indexes its document. An authored node -
// a criterion, a requirement, a check or a plan task - is the one block the render does not place:
// it takes its first parent from what its author wrote, its parent attribute, its serves pointer
// and for a requirement its From: line (located), and keeps it until its parent attribute names
// another target (placeAuthored). Every block the pass places takes its rank, its text, its
// placement and a plan node's size as values, and its place in its document's text as a
// text_parent tuple and a text_rank value (planBlockValues), and an issue room records the
// document of each of its blocks (planRoomBlocks).
//
// version.Pointers are its `block[]` values (pmdoc.BlockPointers, which lists none of a typed
// block repeating the id of a typed block before it): each whose block is a node is an edge of its
// attribute's kind from that block, carrying the attribute's targets as to_types, which the
// readers list only while its target has an allowed type (block_ref_listed). A value is written in
// its typed block's opening line, so the extraction also finds it as a mention held by that block:
// the block's mention of a target is kept only while the block itself holds more references to it
// than its pointers do (a string attribute citing it too, say). A pointer's far end is the node it
// resolves to now (resolvePointerPairs).
//
// version.Typed is what the typed-node pass read of the version (DecisionFacts; the zero TypedFacts for a
// caller that reads none, which writes every decision and reading as its type's node and no tie
// of theirs): a decision's and a reading's node takes its type's supertag, a reading in an
// answered ask takes that ask's node for its part_of, and the pairs the plan reconciles carry
// each decision's from_ask and decided_by and each ask block's shaped, marked with the ask's
// node, and none of their ties to another issue (TypedFacts.pairs). So every block_refs row of
// the document has this one writer, the typed-node pass's above the deferral bound included
// (PlanDecisionNodes), and the two write one set of rows for one version. The typed-node pass's
// own facts of the version - the node of each decision and reading the store holds otherwise
// than the version leaves it, each one's author and each decision's values - are planned here
// too, into the same write (TypedFacts.indexedFacts), since the two passes run over one version
// together and one write of the store takes each node once.
//
// A block whose text moved keeps its changed version and time as well where another renderer
// version stored its changed version, or none stamped it (pmdoc.OtherRenderer), and the text
// recorded for the block there is that renderer's spelling of its tree here. Of the blocks whose
// text moved and whose changed version such a renderer version stored - for a block of an issue
// room, a block still of the document whose version that is, since a block that moved into another
// document records that document's version - the plan names those aloneCandidates keeps as its
// candidates, and reads nothing for them: KeepRespelled, which the caller runs in its read phase
// right after the plan, reads their recorded texts and the stored versions of their documents and
// decides (respelledBlocks). Each block it keeps has its row take this text's sha256 and excerpt,
// and no text is recorded for it at this version: a pin to an earlier version still reads
// unchanged, and the block's recorded text keeps the earlier spelling until its text next changes.
// Such a block counts as unchanged in Moved and Cited. Its row is still one the pass writes, so its
// node is stated live as every such block's is (blockNode), which moves nothing of a block the
// version holds. Every other block whose text moved takes the version its text stands since
// (textSince).
//
// What the pass states in the node and tuple store (Stated) is held for the event that caused the
// write (graphstore.Uncaused): the block nodes it writes, and the tuples the projection rows
// beside them are the projection of. The nodes are written by the pass itself, first among its
// statements after the fence, because the foreign key on (node_kind, node_address) is checked
// inside each statement that writes a projection row and there is no node to name otherwise; the
// tuples are not, because on the request path no event exists while the pass runs. The caller
// appends them once it has queued that event, in the same transaction, right after it, where
// refs.QueueStamp runs. The base attributes of the nodes the pass wrote ride with them, so a block
// stated gone by a version carries the event that deleted it, and a block_refs row the pass
// inserts outside a first indexing takes that event as its tuple's legacy_source_seq
// (graphstore.UncausedTuple). A pass that lost the revision fence states nothing and answers the
// zero write, for which graphstore.QueueCaused writes nothing. Each block whose words the version
// added, changed or restored, and each it dropped, is marked changed (graphstore.Node's Changed),
// so where the caller appends the write on a write phase made for a run of the command bar
// (store.NewWriteBatch over a context store.WithRun gave the run), each of them, and no other
// block, carries that run's changed_by_run (graphstore.QueueCaused).
//
// The revision fence (queueStand) is decided here on the stands the index holds: a pass over one
// document wins where its document's row admits its version (blockStand.admits), and a pass over
// an issue room where any document's row it covers admits the version it covers of that
// document, and then sets every one of them forward. A block of the document whose row names no
// node - a row a build before this release wrote, which the backfill has not reached - takes its
// node and its membership of the document here, whether this version touched its text or not,
// because the row learns the address of that node in this same pass (the parents' projection) and
// the foreign key on it is checked inside the statement that writes it.
//
// A pass over a document that holds this build's mark (store_owed_at, blockStand.owed) - or over
// an issue room any of whose documents holds it - is the drain of that mark (Marked), and it names
// the node of every block_refs row of the document that names none as well, stating the link that
// row projects (queueBlockRefs). The mark is set where this build leaves the document's blocks to a
// later pass - the backfill, a deferred version, a re-index owed - and the release before this one
// can index the document before that pass runs, which writes its rows with no node. A row of a
// pair that survives is what that release leaves, and a pass that named a row's node only when it
// inserted the row would leave it naming none until someone ran the backfill again. The drain is
// not the only pass that meets such a row: a pass that reads the store's statement as stale names
// it too (below).
//
// Where another release indexed the document after the store last stated its blocks
// (blockStand.stale, of artifactID's own row), the rows the pass compares the render with are that
// release's, and they no longer name everything the store holds: the release before this one
// deletes the rows of the blocks and the links its version drops, inserts the rows of those it adds
// naming no node, and states no node gone, retracts no tuple and asserts none. The pass then also
// reads the store's own statement of the document
// (heldDocumentBlocks) and states, from the render, what a version dropping those rows would have
// stated: each block the store holds present that the version does not hold, gone with its
// pointers (goneSinceStated, retractGonePointers), and each link the store asserts out of a block
// of the document, of a kind a pair of a version writes (heldLinkKinds), that no pair of the
// version writes, retracted (unheldLinks). A tuple another writer states out of a block - a
// requirement's constrained_by, a changed_by_run - is no such link, and the pass leaves it where
// it stands. And since that release rewrote the row of each block whose text its version moved
// and stated no text value, the pass states the text of every block it places (planBlockValues).
// That is no repair: what decides each statement is the version's render, and the store is read
// only for what it would go on asserting against it, as planBlockParents reads it for the
// parents. Such a pass also names the node of every block_refs row that survives naming none,
// stating the link that row projects, as the drain does, whether or not the document holds the
// mark: a version this build writes before any tick of this build reads the event of that
// release's version is such a pass, and it moves store_version to its own version, after which no
// tick marks the document (docs' markVersionedDocuments) and no drain reaches the row. A pass over
// a document only this release indexed reads none of it.
//
// version.Beside is what the caller states under the same event and has the pass write with its own (the
// person who removed an answered ask, and the removal's facts): a write planned with
// graphstore.PlanUncaused that queues nothing of its own. Its nodes go in the pass's one node
// write, after the pass's own and without a node the pass already writes, so the QueueCaused of
// Stated moves them and states their base attributes, and its tuples are appended to the pass's
// facts as they stand. A pass that loses the fence states Beside alone, and Queue writes its
// nodes and nothing else. The zero Beside changes nothing.
func PlanBlocks(ctx context.Context, q Queryer, index BlockIndex, rule int, now time.Time, version VersionBlocks) (BlockIndexing, error) {
	artifactID, record, versions := version.ArtifactID, version.Record, version.Versions
	markdown, blocks, located, pointers := version.Markdown, version.Blocks, version.Located, version.Pointers
	typed, beside := version.Typed, version.Beside
	// The version the facts that name no block of their own are stated in the name of: the one
	// version of a document, or the newest of an issue room's.
	lead, newest := versions.Newest(artifactID)
	number, versionAt := newest.Number, newest.At
	indexing := BlockIndexing{
		artifactID: artifactID, refKey: record.RefKey, version: number, versionAt: versionAt,
		versions: versions, rule: rule, held: index,
	}
	// The stand of each document the pass covers: artifactID's own the index holds, and each other
	// document's of an issue room as the transaction's earlier indexing of the room left it
	// (BlockIndex.stands), else as read here.
	stands := map[string]blockStand{artifactID: index.stand}
	var unread []string
	for _, document := range slices.Sorted(maps.Keys(versions)) {
		if document == artifactID {
			continue
		}
		if stand, held := index.stands[document]; held {
			stands[document] = stand
			continue
		}
		unread = append(unread, document)
	}
	if len(unread) > 0 {
		read, err := readBlockStands(ctx, q, unread)
		if err != nil {
			return BlockIndexing{}, err
		}
		for _, document := range unread {
			stands[document] = read[document]
		}
	}
	// The fence decides first, so a pass that loses it plans nothing and writes nothing of its own:
	// the pass wins where the row of any document it covers admits it, artifactID's own among them
	// (stands holds index.stand).
	won, marked := false, false
	for document, covered := range versions {
		won = won || stands[document].admits(covered.Number, rule)
		marked = marked || stands[document].owed
	}
	if !won {
		if len(beside.Nodes) > 0 || len(beside.Tuples) > 0 {
			indexing.stated = graphstore.PlanUncaused(beside.Nodes, nil)
			indexing.stated.Tuples = append(indexing.stated.Tuples, beside.Tuples...)
		}
		return indexing, nil
	}
	stale := index.stand.stale()
	indexing.won, indexing.marked, indexing.namesSurvivingRows = true, marked, marked || stale
	if record.At.IsZero() {
		if err := q.QueryRow(ctx, `select clock_timestamp()`).Scan(&record.At); err != nil {
			return BlockIndexing{}, fmt.Errorf("read the time of the block index of %s: %w", artifactID, err)
		}
	}
	refKey, writer := record.RefKey, record.Writer
	stored := maps.Clone(index.blocks)
	indexing.firstIndexing = len(stored) == 0
	nodes, long, longest := uniqueBlocks(blocks)
	// The blocks past the bound are no nodes, but an issue room records the document of each, as it
	// does every other block's (planRoomBlocks).
	var pastBound []model.ArtifactBlock
	if long > 0 {
		pastBound = pastBoundBlocks(blocks)
		slog.Warn("dispatch: block ids past the bound are not graph nodes; a mention inside one of their blocks starts at the block holding it",
			"artifact", artifactID, "version", number, "blocks", long, "longest_bytes", longest, "bound_bytes", text.MaxBlockIDBytes)
	}
	fits, unaddressable := blockFits(refKey, nodes)
	if unaddressable > 0 {
		slog.Warn("dispatch: block addresses past the bound are no nodes of the store; their rows carry no node address and the pass states no fact about them",
			"artifact", artifactID, "version", number, "blocks", unaddressable, "ref_key_bytes", len(refKey), "bound_bytes", MaxBlockNodeBytes)
	}
	// The documents whose fragments of an issue room hold this version's blocks, where the
	// document indexed is an issue's tree artifact: each block's in_document names its fragment's
	// document, and its rank stands in that document's band.
	fragmented := nodes
	if len(pastBound) > 0 {
		fragmented = slices.Concat(nodes, pastBound)
	}
	fragments, err := fragmentDocuments(ctx, q, artifactID, fragmented)
	if err != nil {
		return BlockIndexing{}, err
	}
	var holders map[string]model.ArtifactBlock
	if len(fragments) > 0 {
		holders = make(map[string]model.ArtifactBlock, len(nodes))
		for _, block := range nodes {
			holders[block.ID] = block
		}
	}
	// The document each block is a block of - the one whose fragment of an issue room holds it,
	// which the pass read (fragmentDocuments), or the one its row held for a block the version
	// dropped, and artifactID itself for a block of no other - and the version it records, that
	// document's (versionOf).
	documentOf := func(id string) string {
		if held, ok := stored[id]; ok && held.document != "" {
			return held.document
		}
		if block, ok := holders[id]; ok {
			if _, known := fragments[block.Document]; known {
				return block.Document
			}
		}
		return artifactID
	}
	note := func(id string) {
		if document := documentOf(id); document != artifactID {
			if indexing.documentOf == nil {
				indexing.documentOf = map[string]string{}
			}
			indexing.documentOf[id] = document
		}
	}
	versionIn := func(document string) Version {
		_, version := versions.of(document, artifactID)
		return version
	}

	// The blocks whose text this version changed (new, changed or restored), and those it
	// deleted. held is each block's row as the indexing leaves it, which a later indexing of the
	// document in this transaction plans against (Held): a row the pass writes names its node
	// wherever its address fits, since the parents' projection names it on every placed row.
	// A block the index holds whose changed version another renderer version stored, or none
	// stamped, may hold its tree in that renderer's spelling, and is a candidate for
	// KeepRespelled; every other block whose text moved takes this version, which this build
	// renders, so the index holds it as respellable no longer.
	held := make(map[string]storedBlock, len(nodes))
	respellable := maps.Clone(index.respellable)
	// Which of the changed blocks holds no row yet, so this pass writes its row first: a block the
	// version adds, or one a version restores, whose row went with the version that dropped it.
	var first []bool
	// The blocks of this version whose row names no node, each with the time that row was
	// written: a pre-store row, which the pass below gives its node and its membership.
	var nodeless []nodelessBlock
	// The blocks of this version whose row the version leaves as it stands and whose node carries
	// another supertag than the block's type gives, each with that supertag: the pass restates
	// their nodes and nothing else of them.
	var retagged []retaggedBlock
	// The blocks whose row the pass inserts, which join the document here and state their
	// membership of it, dated as the insert dates the row: by its column's default, now.
	inserted := make(map[string]struct{})
	// A block whose text no version moved needs no string of its sum: the comparison against
	// the sum the index holds reads the hex out of one buffer this loop keeps, and only a
	// block this version writes a row for takes a string of its own.
	var sum blockSumHex
	for position, block := range nodes {
		body := markdown[block.From:block.To]
		blockTextSum(&sum, body)
		previous, kept := stored[block.ID]
		delete(stored, block.ID)
		note(block.ID)
		if kept && previous.node == "" {
			nodeless = append(nodeless, nodelessBlock{id: block.ID, typ: block.Type, joined: previous.joined})
		}
		document, was := documentOf(block.ID), previous.document
		if was == "" {
			was = artifactID
		}
		// A block of an issue room cut from one fragment and pasted with the same text into
		// another moved into another document, whose version it records from here on: its row
		// and its text are written again at that document's version, as a changed block's are.
		if kept && previous.sum == string(sum[:]) && was == document {
			// A block whose node carries another supertag than its type gives (R26 of
			// the issue-tree spec) - one a build before this one wrote as block, or one
			// the backfill wrote before the facts that decide a reading were read - has its node
			// written again with that supertag, its row, its text and its changed_version as they
			// stand, as a block that only moved keeps them. An index that changes no type writes
			// no node for it.
			supertag := previous.supertag
			switch {
			case !fits[position]:
			case previous.node == "":
				// The pass names this row's node below (nodeless), under the supertag its type gives.
				supertag = typed.supertag(block.ID, block.Type)
			case typed.decides(block.Type):
				if want := typed.supertag(block.ID, block.Type); want != previous.supertag {
					retagged = append(retagged, retaggedBlock{id: block.ID, supertag: want})
					supertag = want
				}
			}
			held[block.ID] = storedBlock{sum: previous.sum, joined: previous.joined, node: namedNode(refKey, block.ID, fits[position]), supertag: supertag, document: document}
			continue
		}
		hexSum := string(sum[:])
		joined := now
		if kept {
			joined = previous.joined
		} else {
			inserted[block.ID] = struct{}{}
		}
		var supertag string
		if fits[position] {
			supertag = typed.supertag(block.ID, block.Type)
		}
		held[block.ID] = storedBlock{sum: hexSum, joined: joined, node: namedNode(refKey, block.ID, fits[position]), supertag: supertag, document: document}
		excerpt := BlockExcerpt(body, version.ServerURL)
		// A block of an issue room that moved into another document records that document's
		// version from here on, whatever spelling its text had there, so only a block that stays
		// a block of its document is a candidate.
		if storedAt, other := respellable[block.ID]; other {
			if was == document {
				indexing.candidates = append(indexing.candidates, respellCandidate{block: block, document: document, body: body, sum: hexSum, excerpt: excerpt, storedAt: storedAt})
			}
			delete(respellable, block.ID)
		}
		indexing.ids, indexing.types, indexing.sums = append(indexing.ids, block.ID), append(indexing.types, block.Type), append(indexing.sums, hexSum)
		first = append(first, !kept)
		indexing.excerpts = append(indexing.excerpts, excerpt)
		indexing.textIDs, indexing.texts = append(indexing.textIDs, block.ID), append(indexing.texts, &body)
	}
	// A block of a retired document of an issue room that no fragment of the room holds now is no
	// block this version dropped: the pass keeps it as the retirement left it (retained).
	for id, block := range stored {
		if block.retired {
			held[id] = block
			indexing.retained = append(indexing.retained, id)
			delete(stored, id)
		}
	}
	slices.Sort(indexing.retained)
	// In id order, so the rows this pass writes and the facts it states come in an order a
	// reader of the log can predict, which a map's iteration would not give.
	indexing.deleted = slices.Sorted(maps.Keys(stored))
	for _, id := range indexing.deleted {
		note(id)
		indexing.textIDs, indexing.texts = append(indexing.textIDs, id), append(indexing.texts, nil)
		delete(respellable, id)
	}
	versionOf := indexing.versionOf
	// A changed block's row and its text are recorded at the version its text stands since
	// (textSince), and a deleted block's text at the version of its document the pass covers, so
	// the text a reader reads at an indexed version is the latest row at or before it, as ever, and
	// the row a block's changed_version names.
	indexing.textVersions = make([]int32, len(indexing.textIDs))
	for position, id := range indexing.textIDs {
		indexing.textVersions[position] = int32(versionOf(id).Number)
	}
	// The version each block whose row this pass writes stands since (textSince): this version,
	// or where the pass covers versions no pass indexed, the earliest of them from which its text
	// stood, which is its row's changed_version and, for a block whose row the pass writes first,
	// the version that first holds it. Its authors are who made such a block: the first of them
	// its node's made_by, at that version's time, and each of them, in the version's own order, an
	// authored_by of a node that records its authors (blockAuthors). A block of an issue room
	// stands since a version of its own document (documentOf).
	authors := map[documentVersion][]model.Actor{{document: lead, number: number}: record.Authors}
	if len(indexing.ids) > 0 {
		before := make(map[string]int, len(versions))
		for document, covered := range versions {
			if before[document], err = indexedBefore(ctx, q, document, covered.Number, stands[document]); err != nil {
				return BlockIndexing{}, err
			}
		}
		if indexing.since, indexing.sinceAt, err = textSinceIn(ctx, q, versions, before, indexing.ids, indexing.texts[:len(indexing.ids)], documentOf, versionIn); err != nil {
			return BlockIndexing{}, err
		}
		copy(indexing.textVersions, indexing.since)
		wanted := make([]documentVersion, len(indexing.ids))
		for position, id := range indexing.ids {
			wanted[position] = documentVersion{document: documentOf(id), number: int(indexing.since[position])}
		}
		if authors, err = versionAuthors(ctx, q, authors, wanted); err != nil {
			return BlockIndexing{}, err
		}
	}
	indexing.candidates = aloneCandidates(blocks, indexing.candidates)
	authorsOf := func(position int) []model.Actor {
		return authors[documentVersion{document: documentOf(indexing.ids[position]), number: int(indexing.since[position])}]
	}

	// The nodes this pass states are the blocks whose document_blocks row it writes - the new,
	// the changed and the restored - the ids the version dropped, and the blocks whose row
	// names no node, and no other. The first two are exactly the set R16's foreign key needs a
	// node for where every row of the document already names one, and that is what makes the
	// write cost what the edit changed rather than what the document holds: a version that
	// moves one word of a two-thousand-block document states two node rows, not four thousand.
	// A block whose text this version left alone already has its node from the version that
	// first indexed it, and its placement, which may still have moved, is a tuple
	// planBlockParents states against that node.
	changed := make(map[string]struct{}, len(indexing.ids))
	for _, id := range indexing.ids {
		changed[id] = struct{}{}
	}
	// Where the store's statement of the document is stale, a block another release's version
	// dropped has no row left for deleted to name, and its node stands: the store's own
	// statement of the document names it (goneSinceStated), and it is stated gone by the rule
	// below, its pointers with it, as a block whose row the version dropped is. It takes no
	// document_block_texts row and moves no panel (textIDs), since that release deleted its row
	// and moved what the row showed. Every address the store holds is inside the bound, since a
	// block past it is no node.
	var statement map[string][]heldLink
	dropped := indexing.deleted
	if stale {
		if statement, err = heldDocumentBlocks(ctx, q, refKey); err != nil {
			return BlockIndexing{}, err
		}
		dropped = slices.Concat(indexing.deleted, goneSinceStated(statement, nodes, indexing.deleted))
	}
	// Every node the pass writes restates its block (blockNode): a block this version holds is
	// written live and keeps a state a write marked it with, so a version that changes a
	// proposed requirement's words leaves it proposed, and comes back live only from gone. A
	// block whose words this version wrote and a block it dropped are what the version changed,
	// so each is marked changed, and a version written for a run names it as changed by that run
	// (graphstore.TupleChangedByRun) where its cause is appended (graphstore.QueueCaused).
	blockNodes := make([]graphstore.Node, 0, len(indexing.ids)+len(dropped)+len(nodeless)+len(retagged))
	// The authors of the version each block whose row this pass writes first stands since, by id:
	// that version first holds it, and no document_block_texts row says so before this pass
	// writes one (blockAuthors).
	var firstWritten map[string][]model.Actor
	for position, id := range indexing.ids {
		if !blockIDFits(refKey, id) {
			continue
		}
		made := authorsOf(position)
		var maker model.Actor
		if len(made) > 0 {
			maker = made[0]
		}
		blockNodes = append(blockNodes, blockNode(blockNodeAddress(refKey, id), record, maker, indexing.sinceAt[position], graphstore.StateLive, typed.supertag(id, indexing.types[position]), true))
		if first[position] {
			if firstWritten == nil {
				firstWritten = map[string][]model.Actor{}
			}
			firstWritten[id] = made
		}
	}
	// Every block of the version whose node records its authors, whether this version wrote it
	// or not, so a node an earlier build wrote takes its authors at the pass that next indexes
	// its document. A block an earlier indexing of this transaction already settled the authors
	// of (BlockIndex.authored) is none of them: what it stated is queued, and no read of the store
	// sees it yet.
	var recording []string
	for position, block := range nodes {
		if _, settled := index.authored[block.ID]; !settled && fits[position] && recordsAuthors(block.Type, record.Band) {
			recording = append(recording, block.ID)
		}
	}
	authorNodes, authored, err := blockAuthors(ctx, q, artifactID, record, recording, func(id string) []model.Actor { return firstWritten[id] })
	if err != nil {
		return BlockIndexing{}, err
	}
	blockNodes = append(blockNodes, authorNodes...)
	// A block the version no longer holds has its node stated gone, whatever state it held: a
	// restating write that says gone is written gone, and keeps only the visibility. The tuple
	// that says so is not built here: the node write states a base attribute it moved, with this
	// write's own time and writer, so a block restored by a later version is stated live by the
	// same rule and no writer of this store states a base attribute of its own. Its node keeps the
	// supertag it holds, which no version holding the block is left to say. A block this version
	// dropped is one it changed (blockNode's changed); one another release's version dropped is
	// not.
	supertags, err := heldSupertags(ctx, q, refKey, dropped)
	if err != nil {
		return BlockIndexing{}, err
	}
	gone := make([]string, 0, len(dropped))
	for position, id := range dropped {
		if !blockIDFits(refKey, id) {
			continue
		}
		supertag, known := supertags[id]
		if !known {
			supertag = BlockSupertag
		}
		gone = append(gone, blockNodeAddress(refKey, id))
		blockNodes = append(blockNodes, blockNode(blockNodeAddress(refKey, id), record, writer, versionOf(id).At, graphstore.StateGone, supertag, position < len(indexing.deleted)))
	}
	// A block whose words the version left as they stand, whose node carries another supertag than
	// its type gives, is restated under that supertag (retagged), keeping its state, its
	// visibility and its maker (blockNode). It is no block this version changed.
	for _, block := range retagged {
		blockNodes = append(blockNodes, blockNode(blockNodeAddress(refKey, block.id), record, writer, versionOf(block.id).At, graphstore.StateLive, block.supertag, false))
	}
	// A row written before this release names no node: migration 20261109022248 added the
	// columns and filled none of them, and the backfill has not reached the document or has
	// not been run at all. The parents' projection names this block's node on every row of the
	// document whose columns differ, and document_blocks_node is checked inside that statement,
	// so each of those rows takes its node here or that update is refused. The membership goes
	// with it: the insert states one only for a row it truly inserts, so a pre-store block whose
	// text this version changed would otherwise keep its row, state no in_document, and be
	// skipped by the backfill for naming a node by then.
	//
	// Each membership is dated from its row's own created_at, as a row the insert writes afresh
	// is, so the tuple arm of graph_edges answers what the document_blocks arm answered for it, and
	// names the document whose fragment of an issue room holds the block (inDocument). A block
	// past the address bound is no node of the store: it takes neither, and its row keeps its null
	// columns, which is what keeps the old arm answering it.
	var memberships []graphstore.UncausedTuple
	for _, block := range nodeless {
		if !blockIDFits(refKey, block.id) {
			continue
		}
		address := blockNodeAddress(refKey, block.id)
		// A block whose row only learns its node here is no block this version changed: it is
		// not marked (blockNode).
		if _, stated := changed[block.id]; !stated {
			blockNodes = append(blockNodes, blockNode(address, record, writer, versionOf(block.id).At, graphstore.StateLive, typed.supertag(block.id, block.typ), false))
		}
		memberships = append(memberships, membership(address, record.inDocument(holders[block.id], fragments), block.joined, writer))
	}
	// The typed-node pass's own nodes of the version, and its own facts of them, which go in one
	// write with the block index's (TypedFacts.indexedFacts): a node the index writes already is
	// not written twice.
	typedNodes, typedTuples := typed.indexedFacts(blockNodes)
	written := slices.Concat(blockNodes, typedNodes)
	// The nodes go in before any projection row that names one: the foreign key is checked
	// inside the statement writing that row, so a node written afterwards would be too late.
	// The base attributes that write states are appended with the pass's own tuples under the
	// caller's event (graphstore.Uncaused), as the first of them. They carry no
	// legacy_source_seq: no legacy row states a node's visibility or its state. The pass states
	// each fact of a block once, so the tuples of a block node its own node write makes are
	// appended with no conflict arm (graphstore.ArmlessFresh), and every other tuple keeps it.
	indexing.stated = graphstore.ArmlessFresh(graphstore.PlanUncaused(append(written, unwritten(beside.Nodes, written)...), nil), BlockNodeKind)
	indexing.stated.Tuples = append(indexing.stated.Tuples, memberships...)
	indexing.stated.Tuples = append(indexing.stated.Tuples, authored...)
	// The pointer tuples at the blocks it states gone go with them (retractGonePointers): read
	// here, in the plan, and appended beside the facts that node write states, under the same
	// cause, in the same write phase as the node write that states each block gone.
	retracted, err := retractGonePointers(ctx, q, gone, versionAt, writer)
	if err != nil {
		return BlockIndexing{}, fmt.Errorf("retract the pointers at the blocks version %d of %s no longer holds: %w", number, artifactID, err)
	}
	indexing.stated.Tuples = append(indexing.stated.Tuples, retracted...)
	// A block whose row the insert writes afresh joins the document there; one whose row was
	// already there - a block whose text alone changed - keeps that row and the membership it
	// already states.
	for _, id := range indexing.ids {
		if _, fresh := inserted[id]; !fresh || !blockIDFits(refKey, id) {
			continue
		}
		indexing.stated.Tuples = append(indexing.stated.Tuples, membership(blockNodeAddress(refKey, id), record.inDocument(holders[id], fragments), now, writer))
	}
	// An issue room's blocks name their documents in issue_room_blocks, beside their
	// document_blocks rows under the tree artifact, in this transaction.
	moves, rooms, err := indexing.planRoomBlocks(ctx, q, index, record, nodes, fits, pastBound, fragments)
	if err != nil {
		return BlockIndexing{}, err
	}
	indexing.stated.Tuples = append(indexing.stated.Tuples, moves...)
	sources := authoredSourcesOf(markdown, nodes, located, pointers)
	placement, parents, placed, err := indexing.planBlockParents(ctx, q, index, record, nodes, fits, fragments, typed.asks(), nil, sources)
	if err != nil {
		return BlockIndexing{}, err
	}
	indexing.stated.Tuples = append(indexing.stated.Tuples, parents...)
	values, heldValues, err := planBlockValues(ctx, q, index, record, markdown, nodes, placement, changed, stale, fragments)
	if err != nil {
		return BlockIndexing{}, err
	}
	indexing.stated.Tuples = append(indexing.stated.Tuples, values...)

	// The links the version drops, in the order the index listed them, then those it adds and
	// those whose block types it changes, in the order the insert writes them: the targets the
	// two statements stand for. Then the target of every link the version writes in a block whose
	// text it changed, whose excerpt moved. Each pointer's far end is the node it resolves to now.
	addressed := make([]string, 0, len(nodes))
	for position, node := range nodes {
		if fits[position] {
			addressed = append(addressed, blockNodeAddress(refKey, node.ID))
		}
	}
	if indexing.pairs, err = resolvePointerPairs(ctx, q, artifactID, addressed, typed.pairs(blockPairs(nodes, located, typed.withTies(pointers)))); err != nil {
		return BlockIndexing{}, err
	}
	links, heldLinks, err := indexing.planBlockRefs(ctx, q, index, writer)
	if err != nil {
		return BlockIndexing{}, err
	}
	indexing.stated.Tuples = append(indexing.stated.Tuples, links...)
	// Where the store's statement is stale, the links another release's version dropped have
	// no row left for the delete to drop, and the store still asserts them (unheldLinks). They
	// move no count and no panel, and neither do the blocks goneSinceStated names: what a reader
	// is shown of a block and its links is read through their rows (graph_edges answers a
	// block's tuple only while its document_blocks or block_refs row stands), and that release
	// deleted those rows, and moved what they showed, when it wrote its version.
	indexing.stated.Tuples = append(indexing.stated.Tuples, unheldLinks(statement, refKey, indexing.pairs, links, versionAt, writer)...)
	// A pair held by a block whose text this version moved is a link whose excerpt moved with
	// it: the blocks the nodes above were stated for, until KeepRespelled takes the blocks it
	// keeps out of that set and sets these targets again (moveChangedBlockLinks).
	indexing.linkMoves = len(indexing.moved)
	indexing.moveChangedBlockLinks(changed)
	// The typed-node pass's values and authors come last, as its write followed the index's, and
	// after them the caller's own facts beside the pass.
	for _, tuple := range typedTuples {
		indexing.stated.Tuples = append(indexing.stated.Tuples, graphstore.UncausedTuple{Tuple: tuple})
	}
	indexing.stated.Tuples = append(indexing.stated.Tuples, beside.Tuples...)
	// What the index holds once the pass is written: its rows, links and placements, its values,
	// its room's record, the blocks whose authors it settled, and every document it covers stood
	// at the version the pass covers of it.
	settled := maps.Clone(index.authored)
	if settled == nil && len(recording) > 0 {
		settled = make(map[string]struct{}, len(recording))
	}
	for _, id := range recording {
		settled[id] = struct{}{}
	}
	// A pass over one document wins its row forward (RevisionFenceStatement); the statement of a
	// pass over an issue room never moves a row back, the tree's own among them (queueStand).
	stood := index.stand
	if own, ok := versions[artifactID]; ok && len(versions) == 1 {
		stood = index.stand.indexedAt(own.Number, rule)
	} else if ok {
		stood = index.stand.indexedUpTo(own.Number, rule)
	}
	var heldStands map[string]blockStand
	for document, covered := range versions {
		if document == artifactID {
			continue
		}
		if heldStands == nil {
			heldStands = make(map[string]blockStand, len(index.stands)+len(versions))
			maps.Copy(heldStands, index.stands)
		}
		heldStands[document] = stands[document].indexedUpTo(covered.Number, rule)
	}
	if heldStands == nil {
		heldStands = index.stands
	}
	indexing.held = BlockIndex{
		blocks: held, respellable: respellable, links: heldLinks, stand: stood, placed: placed,
		stands: heldStands, values: heldValues, rooms: rooms, authored: settled,
	}
	return indexing, nil
}

// moveChangedBlockLinks sets the targets Moved answers after the first linkMoves, those of the
// links the version inserts, deletes, retypes or rehomes: the target of every link the version
// writes in a block whose text it changed (changed), whose excerpt moved.
func (b *BlockIndexing) moveChangedBlockLinks(changed map[string]struct{}) {
	b.moved = b.moved[:b.linkMoves]
	for _, pair := range b.pairs {
		if _, wrote := changed[pair.blockID]; wrote {
			b.moved = append(b.moved, pair.target)
		}
	}
}

// KeepRespelled decides which of the plan's candidates hold the text recorded for them at their
// changed version in another renderer version's spelling of the same tree (respelledBlocks),
// reading through q, and takes each it keeps out of the blocks the version changes: its row keeps
// its changed version and time and takes this text's sha256 and excerpt (Queue), no text is
// recorded for it (textIDs, texts and textVersions leave it out, and ids, since and sinceAt with
// the rest of the changed rows), a link it holds is not counted for its excerpt, and the index
// holds it as respellable still (Held). It is the read half of the plan, run in the caller's read
// phase right after PlanBlocks and before every other use of the indexing (Moved, Cited, Held,
// Queue), so the parse and render it makes (pmdoc.FragmentReadsAs) hold no transaction id. It
// reads nothing when the plan named no candidate (aloneCandidates), as for a pass that lost the
// revision fence, so a version that moves no such block pays nothing for it.
func (b *BlockIndexing) KeepRespelled(ctx context.Context, q Queryer) error {
	kept, err := respelledBlocks(ctx, q, b.artifactID, b.candidates)
	b.candidates = nil
	if err != nil || len(kept) == 0 {
		return err
	}
	keep := make(map[string]struct{}, len(kept))
	for _, candidate := range kept {
		keep[candidate.block.ID] = struct{}{}
		b.respelledIDs, b.respelledSums = append(b.respelledIDs, candidate.block.ID), append(b.respelledSums, candidate.sum)
		b.respelledExcerpts = append(b.respelledExcerpts, candidate.excerpt)
		b.held.respellable[candidate.block.ID] = candidate.storedAt
	}
	changed := make(map[string]struct{}, len(b.ids))
	for index, id := range b.ids {
		if _, respelled := keep[id]; respelled {
			continue
		}
		at := len(changed)
		b.ids[at], b.types[at], b.sums[at], b.excerpts[at] = id, b.types[index], b.sums[index], b.excerpts[index]
		b.since[at], b.sinceAt[at] = b.since[index], b.sinceAt[index]
		changed[id] = struct{}{}
	}
	b.ids, b.types, b.sums, b.excerpts = b.ids[:len(changed)], b.types[:len(changed)], b.sums[:len(changed)], b.excerpts[:len(changed)]
	b.since, b.sinceAt = b.since[:len(changed)], b.sinceAt[:len(changed)]
	recorded := 0
	for index, id := range b.textIDs {
		if _, respelled := keep[id]; !respelled {
			b.textIDs[recorded], b.texts[recorded], b.textVersions[recorded] = id, b.texts[index], b.textVersions[index]
			recorded++
		}
	}
	b.textIDs, b.texts, b.textVersions = b.textIDs[:recorded], b.texts[:recorded], b.textVersions[:recorded]
	b.moveChangedBlockLinks(changed)
	return nil
}

// Moved is the targets of the links the indexing inserts or deletes, of those whose block types it
// changes and of those whose node it moves (rehome), whose counts move - a node is counted once
// for each block citing it - and of the links it writes in a block whose text it changes, whose
// excerpts move, a link's excerpt being the text of the block holding it.
func (b BlockIndexing) Moved() []Target {
	return b.moved
}

// Won says the indexing won the revision fence: a pass that lost it - one holding the same
// version as the document's blocks stand at, or a lower one - writes nothing.
func (b BlockIndexing) Won() bool {
	return b.won
}

// Marked says the indexing drains this build's mark (store_owed_at, blockStand.owed) of
// artifactID, or of any document of the issue room it covers a version of. The release before
// this one writes no version node either, so a version it wrote while the mark stood holds none,
// and the caller, which knows which version it is writing itself, writes the nodes those
// documents' other versions are owed in this same transaction (docs' indexRenderedVersion), since
// no later pass reaches them once the mark is cleared. An indexing that lost the fence drains
// nothing.
func (b BlockIndexing) Marked() bool {
	return b.won && b.marked
}

// Corrects says the indexing states over what another release left of the documents it covers:
// it drains this build's mark (Marked), or it reads the store's statement of the document as
// stale (blockStand.stale), as the release before leaves a document it writes and indexes after
// this build stated it, setting no mark. Either way the versions that release wrote hold no
// version node, and the caller writes their nodes in this same transaction (docs'
// indexRenderedVersion): once this pass commits, the document stands stated at its version by
// this build's rule, so neither a later pass nor the block-index tick's marking reaches them. It
// is the condition the pass names the surviving rows of the document on (namesSurvivingRows).
func (b BlockIndexing) Corrects() bool {
	return b.won && b.namesSurvivingRows
}

// Stated is what the indexing states in the node and tuple store, held for the event that causes
// its write: the caller hands it to graphstore.QueueCaused right after that event, in the same
// write phase. A pass that lost the fence states only what the caller had it state beside it
// (PlanBlocks' beside), the zero write where that is none.
func (b BlockIndexing) Stated() graphstore.Uncaused {
	return b.stated
}

// Cited is, of the blocks the indexing changes, restores or deletes (textIDs), each that a source
// cites (citedBlocks), as q holds them before the indexing is written: such a block's text is what
// every link to it and its own Referenced by panel show, so the version moved what they show. A
// block whose citing link the indexing itself inserts or deletes is among Moved already, so the
// read names the same targets the written index would.
func (b BlockIndexing) Cited(ctx context.Context, q Queryer) ([]Target, error) {
	return citedBlocks(ctx, q, b.artifactID, b.textIDs)
}

// Held is what the document's block index holds once the indexing is written.
func (b BlockIndexing) Held() BlockIndex {
	return b.held
}

// Queue queues the indexing's writes on a write phase's batch, which its caller runs in the
// transaction holding the document's owner lock: the revision fence first (queueStand), then the
// nodes of the blocks it states (graphstore.QueueNodes), then the blocks KeepRespelled kept,
// which take this text's sha256 and excerpt and keep their changed version and time, then the
// blocks the version deleted, which document_blocks loses, and the blocks it changed, with their
// excerpts and their node columns; the text each of those blocks has at its version, recorded in
// document_block_texts, a deleted block's as no text, after which the edges at it stop resolving
// until a version holds its id again; an issue room's record of the document of each block
// (queueRoomBlocks); the parent of each block it places, with its node columns; the links its
// blocks hold; and each version it covers recorded as indexed under its own document
// (document_indexed_versions). It
// queues no window of its own. A pass that lost the fence queues nothing of its own: only the node
// write of what the caller had it state beside it (VersionBlocks.Beside), where there is any. One
// whose row the fence's statement refuses fails the batch there (queueStand). Whether the stand
// clears the document's mark is the stand's own statement's to decide, and none of the statements
// after it reads the mark, so a pass whose stand leaves a mark of a higher rule writes what it
// writes otherwise and no statement of it assumes the mark gone.
//
// A pair (block, target) that survives keeps its created version, time and source event. A new
// pair takes its block's version and that version's time (versionOf), and the source event
// QueueStamp gives it. On the document's first indexing (no block rows yet), a new pair dates
// instead from the document's own refs row for that target, when one exists.
func (b BlockIndexing) Queue(batch *store.WriteBatch) {
	if !b.won {
		graphstore.QueueNodes(batch, b.stated)
		return
	}
	b.queueStand(batch)
	graphstore.QueueNodes(batch, b.stated)
	if len(b.respelledIDs) > 0 {
		batch.Queue(fmt.Sprintf("record the blocks version %d of %s holds respelled", b.version, b.artifactID), `
			update document_blocks d set text_sha256 = r.sum, excerpt = r.excerpt
			from unnest($2::text[], $3::text[], $4::text[]) as r (id, sum, excerpt)
			where d.artifact_id = $1 and d.block_id = r.id and `+graphstore.KeptHeadRows+`
		`, b.artifactID, b.respelledIDs, b.respelledSums, b.respelledExcerpts)
	}
	b.queueRows(batch)
	b.queueRoomBlocks(batch)
	b.queueBlockParents(batch)
	b.queueBlockRefs(batch)
	// Each version is recorded indexed under its own document, which that table keys by.
	documents, numbers := b.versions.numbered()
	batch.Queue(fmt.Sprintf("record the versions of %s indexed", b.artifactID), `
		insert into document_indexed_versions (artifact_id, version)
		select i.artifact_id, i.version from unnest($1::uuid[], $2::int[]) as i (artifact_id, version)
		on conflict (artifact_id, version) do nothing
	`, documents, numbers)
}

// queueRows queues the rows of the blocks the indexing writes, after the node write that states
// them: the blocks it deletes, which document_blocks loses; the blocks whose text it changed, with
// their excerpts, their node columns and the parent the placement gives each (rowParents), so a
// row the insert writes carries its parent and no statement after it touches the row again
// (queueBlockParents); and the text each of those blocks has at its version, recorded in
// document_block_texts, a deleted block's as no text. A block index queues them inside
// its revision fence (Queue), and the typed-node pass of a deferred version queues the rows of the
// typed blocks it removes and of the requirements, checks and plan tasks it writes
// (DecisionIndexing.Queue). The delete and the insert of document_blocks claim the head rows kept
// (graphstore.KeptHeadRows), as every write of the store's own does: document_blocks' trigger
// (issue_head_marks_drop) takes away the mark of each issue whose head rows a write of those rows
// leaves behind, and the pass's cause site writes the blocks' head rows last in the write phase.
//
// A row that stands takes its node columns and its parent in the insert's conflict arm, with its
// text: the projection queueBlockParents makes of a placed row is the same three columns. A block
// whose address is past the store's bound is no node and is placed nowhere, so its row names no
// node and keeps the parent it holds, as the projection, which never reaches it, leaves it.
func (b BlockIndexing) queueRows(batch *store.WriteBatch) {
	artifactID, version := b.artifactID, b.version
	if len(b.deleted) > 0 {
		batch.Queue(fmt.Sprintf("delete the blocks version %d of %s no longer holds", version, artifactID), `
			delete from document_blocks where artifact_id = $1 and block_id = any($2) and `+graphstore.KeptHeadRows+`
		`, artifactID, b.deleted)
	}
	if len(b.ids) > 0 {
		batch.Queue(fmt.Sprintf("record the blocks version %d of %s changed", version, artifactID), `
			insert into document_blocks (artifact_id, block_id, node_id, node_kind, node_address, parent_block_id, type, text_sha256, excerpt, changed_version, changed_at)
			select $1::uuid, b.id, a.ref_key || '#' || b.id,
			       case when octet_length(a.ref_key || '#' || b.id) <= $9 then $8 end,
			       case when octet_length(a.ref_key || '#' || b.id) <= $9 then a.ref_key || '#' || b.id end,
			       b.parent, b.type, b.sum, b.excerpt, b.since, b.since_at
			from unnest($2::text[], $3::text[], $4::text[], $5::text[], $6::int[], $7::timestamptz[], $10::text[])
			     as b (id, type, sum, excerpt, since, since_at, parent)
			cross join (select ref_key from artifacts where id = $1::uuid) a
			where `+graphstore.KeptHeadRows+`
			on conflict (artifact_id, block_id) do update set
				type = excluded.type,
				text_sha256 = excluded.text_sha256,
				excerpt = excluded.excerpt,
				changed_version = excluded.changed_version,
				changed_at = excluded.changed_at,
				node_kind = excluded.node_kind,
				node_address = excluded.node_address,
				parent_block_id = case when excluded.node_kind is null then document_blocks.parent_block_id
				                       else excluded.parent_block_id end
		`, artifactID, b.ids, b.types, b.sums, b.excerpts, b.since, b.sinceAt, BlockNodeKind, MaxBlockNodeBytes, b.rowParents())
	}
	if len(b.textIDs) > 0 {
		batch.Queue(fmt.Sprintf("record the block texts of version %d of %s", version, artifactID), `
			insert into document_block_texts (artifact_id, block_id, version, text)
			select $1::uuid, t.id, t.version, t.text from unnest($2::text[], $3::text[], $4::int[]) as t (id, text, version)
			on conflict (artifact_id, block_id, version) do update set text = excluded.text
		`, artifactID, b.textIDs, b.texts, b.textVersions)
	}
}

// BlockIndexMarkingRule is the version of the marking rule this build applies: what a block-index
// pass and the tick owe, which is which documents a build marks owed a block index
// (store_owed_at) and which comments its tick writes. A change of what a pass owes raises it, and
// TestTheMarkingRuleNamesTheStatementsItMarksWith holds it to the statements that decide it. A
// mark records the rule of the build that set it (store_owed_rule), and so do the block-index
// tick's cursor rows and the backfill's horizons (marking_rule, migration 20261120122600): a build
// starts reading the event log where a build or a backfill of its own rule left off and never
// where one of another rule did, since that build marked what its own rule owes and not what this
// one owes, and a pass of one rule clears no mark of a higher rule. Rule 1 is the first rule this
// code ships, so a build of rule 1 already honours the marks of every later rule. It is declared
// here, where docs and graphcensus both read it: docs' own tests import graphcensus, so graphcensus
// cannot import docs.
//
// Rule 2 is this build's: the facts its pass states of each block of a version beside the block's
// node, its part_of and its parent - its rank, its text, its placement, its place in the text (its
// text_parent and its text_rank), its size and the supertag of its type - which a pass of rule 1
// does not state. A pass records the rule it stated a document by in
// document_block_versions.stated_rule, and the version in stated_version, in the statement
// that writes store_version (queueStand); docs' indexOwedDocument and indexOwedRoom record both
// where they clear the marks of a document no pass covers a version of. The backfill and the
// block-index tick mark owed each document no pass of this rule or a later one has stated at its
// version (UnstatedBlockIndexSQL).
const BlockIndexMarkingRule = 2

// UnstatedBlockIndexSQL is true of the document_block_versions row b where no pass of the marking
// rule rule, a SQL expression, or of a later rule has stated the document at its version: its
// stated_version is not its version, or its stated_rule is below rule, a null on either side
// counted as not equal. store_version cannot say it: a build of an earlier rule writes
// store_version for each version it indexes, and states none of this rule's facts of it. It is
// the one test the backfill (graphcensus's StaleDocumentsStatement and StaleBatchMarkStatement),
// the block-index tick (docs' markVersionedDocuments) and the census's sizing mark or count by,
// each at its build's marking rule (BlockIndexMarkingRule), so a document no pass of that rule has
// stated is marked once by whichever reads it first, and one a pass of that rule has stated since
// is marked by none.
func UnstatedBlockIndexSQL(rule string) string {
	return `(not coalesce(b.stated_version = b.version, false) or not coalesce(b.stated_rule >= ` + rule + `, false))`
}

// IndexBlocks indexes the versions of versions of artifactID's blocks in tx at once: the
// document's index read (ReadBlockIndex) with what the newest version's row says of it
// (ReadIndexedVersion over Versions.Newest), the plan (PlanBlocks), the blocks it keeps across
// another renderer version's spelling (KeepRespelled) and the blocks it cites (Cited), as docs'
// planBlockIndex runs them, and its statements run as a batch of their own (Queue,
// store.RunWrite). It is the whole index for a writer with no write phase to queue it on: the
// seed, and a test that indexes a version itself, each at this build's marking rule
// (BlockIndexMarkingRule).
// versions is the one version a pass over a document indexes (OneVersion), or the version of each
// document an issue room's pass covers (Versions). It answers the targets the indexing moves, the
// cited blocks among them, whether it won the revision fence, what it states in the node and tuple
// store, for the caller to append under its cause (graphstore.Caused), and whether it drained this
// build's mark (Marked). serverURL is the deployment whose dashboard URLs each block's excerpt is
// cut around (BlockExcerpt); typed is what the typed-node pass read of the version
// (DecisionFacts), as PlanBlocks takes it.
func IndexBlocks(
	ctx context.Context,
	tx pgx.Tx,
	artifactID string,
	versions Versions,
	markdown string,
	blocks []model.ArtifactBlock,
	located []text.Located,
	pointers []Pointer,
	serverURL string,
	typed TypedFacts,
) ([]Target, bool, graphstore.Uncaused, bool, error) {
	index, err := ReadBlockIndex(ctx, tx, artifactID)
	if err != nil {
		return nil, false, graphstore.Uncaused{}, false, err
	}
	lead, newest := versions.Newest(artifactID)
	record, err := ReadIndexedVersion(ctx, tx, artifactID, lead, newest.Number)
	if err != nil {
		return nil, false, graphstore.Uncaused{}, false, err
	}
	var now time.Time
	if err := tx.QueryRow(ctx, `select now()`).Scan(&now); err != nil {
		return nil, false, graphstore.Uncaused{}, false, fmt.Errorf("read the transaction's time: %w", err)
	}
	indexing, err := PlanBlocks(ctx, tx, index, BlockIndexMarkingRule, now, VersionBlocks{
		ArtifactID: artifactID, Record: record, Versions: versions,
		Markdown: markdown, Blocks: blocks, Located: located, Pointers: pointers, ServerURL: serverURL, Typed: typed,
	})
	if err != nil {
		return nil, false, graphstore.Uncaused{}, false, err
	}
	if err := indexing.KeepRespelled(ctx, tx); err != nil {
		return nil, false, graphstore.Uncaused{}, false, err
	}
	cited, err := indexing.Cited(ctx, tx)
	if err != nil {
		return nil, false, graphstore.Uncaused{}, false, err
	}
	if err := store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
		indexing.Queue(b)
		return nil
	}); err != nil {
		return nil, false, graphstore.Uncaused{}, false, err
	}
	return slices.Concat(indexing.Moved(), cited), indexing.Won(), indexing.Stated(), indexing.Marked(), nil
}

// CurrentPreventionsSQL is the read of every prevented_by tuple the store asserts now at a block
// whose id is one of the SQL array blocks, each a check that covers a risk, as checked: the newest
// tuple of each relation into those blocks, kept while it asserts (graphstore.CurrentTupleRead).
// columns is what it answers, written over checked's from_kind, from_id, kind, to_kind and to_id.
// Every retraction of such a tuple reads them through PreventionRetractions.
func CurrentPreventionsSQL(columns, blocks string) string {
	return `select ` + columns + `
		from (
			` + graphstore.CurrentTupleRead(`t.to_kind, t.to_id, t.kind, t.from_kind, t.from_id`,
		`t.from_kind, t.from_id, t.kind, t.to_kind, t.to_id, t.asserted`,
		`t.to_kind = '`+BlockNodeKind+`' and t.to_id = any(`+blocks+`) and t.kind = '`+graphstore.TuplePreventedBy+`'`) + `
		) checked
		where checked.asserted`
}

// PreventionRetractions is the retraction of every prevented_by tuple the store asserts now at a
// block of the SQL array blocks, whose parameters are args (CurrentPreventionsSQL), read through
// q before its writer's write phase: each dated at, signed by writer and caused by no event
// (graphstore.NoEvent), in the order of its check and then of its risk. A writer that states them
// under an event causes them by it with the rest of its facts (graphstore.Caused,
// graphstore.QueueCaused). It is a writer's read, which answers no reader and so binds no
// visibility predicate: the one read of the three writers that retract such tuples, each under the
// owner lock (store.LockOwner) of the checks' document it took first - the index pass at the
// blocks a version drops (retractGonePointers, through the plan's own reader), a retirement at the
// blocks of the document it retires (docs' retiredPreventions, in its transaction), and the
// backfill at the checks a process of the release before dropped or retired without retracting
// them (graphcensus' writeEndedPreventions, in its transaction). Each logs what it retracts in its
// own words, and appends the retractions in its write phase, after the read.
func PreventionRetractions(ctx context.Context, q Queryer, blocks string, args []any, at time.Time, writer model.Actor) ([]graphstore.Tuple, error) {
	rows, err := q.Query(ctx, CurrentPreventionsSQL(`checked.from_kind, checked.from_id, checked.to_id`, blocks)+`
		order by checked.to_id, checked.from_kind, checked.from_id`, args...)
	if err != nil {
		return nil, fmt.Errorf("read the prevented_by tuples at the checks: %w", err)
	}
	retracted, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (graphstore.Tuple, error) {
		tuple := graphstore.Tuple{
			Kind: graphstore.TuplePreventedBy, ToKind: BlockNodeKind,
			At: at, Who: writer, SourceEvent: graphstore.NoEvent, Asserted: false,
		}
		err := row.Scan(&tuple.FromKind, &tuple.FromID, &tuple.ToID)
		return tuple, err
	})
	if err != nil {
		return nil, fmt.Errorf("read the prevented_by tuples at the checks: %w", err)
	}
	return retracted, nil
}

// retractGonePointers is the retraction of every pointer tuple a block the version no longer holds
// leaves behind (spec requirement r-constraint-risk): each asserted prevented_by whose
// far end is one of the blocks at addresses, a check that covered a risk (PreventionRetractions),
// and each asserted constrained_by whose near end is one of them, a requirement held to a
// constraint. A constrained_by points at a constraint, never at a block, so the one a gone block
// takes with it starts there. Each retraction is dated at, the version's time, and signed by
// writer, as a dropped pair of block_refs is.
//
// The read is the plan's own (PlanBlocks), through q in the version writer's read phase, and the
// retractions ride with the facts the pass states for the version's event (BlockIndexing.Stated),
// which the writer appends in the same write phase as the node write that states each block gone
// (graphstore.QueueCaused after Queue's graphstore.QueueNodes), so no result is read back between
// two statements of that phase. Nothing writes a pointer at those blocks between the read and the
// write phase: the pass holds the document's owner lock (store.LockOwner) from before its read,
// and a prevented_by write takes the owner lock of its check's document before it reads that
// document (api's lockReasoningOwners). A second indexing of the document in the same transaction
// drops no block the first one dropped, since the first's Held no longer holds it, so it never
// retracts the same pointers twice.
//
// It logs how many it retracts at each block, with the block's address, and answers the number to
// no reader: the risk a check covered may be one only its author may see, and a count served
// beside the block would tell another reader it is there.
//
// A version a delete of the node route writes (R7 of the issue-tree spec: api's DELETE
// /api/v1/issues/{key}/nodes/{id}) names on its context each block that delete takes
// (WithKeptPointerTargets): a prevented_by into such a block is a fact of the risk, a node that
// stays, and stands, so no retraction of one is planned. The constrained_by out of such a block goes
// as out of any other.
func retractGonePointers(ctx context.Context, q Queryer, addresses []string, at time.Time, writer model.Actor) ([]graphstore.UncausedTuple, error) {
	if len(addresses) == 0 {
		return nil, nil
	}
	prevented, err := PreventionRetractions(ctx, q, `$1`, []any{addresses}, at, writer)
	if err != nil {
		return nil, err
	}
	if kept := keptPointerTargets(ctx); len(kept) > 0 {
		prevented = slices.DeleteFunc(prevented, func(tuple graphstore.Tuple) bool { return kept[tuple.ToID] })
	}
	rows, err := q.Query(ctx, `
		select held.from_kind, held.from_id, held.to_kind, held.to_id
		from (
			`+graphstore.CurrentTupleRead(`t.from_kind, t.from_id, t.kind, t.to_kind, t.to_id`, `t.from_kind, t.from_id, t.to_kind, t.to_id, t.asserted`,
		`t.from_kind = '`+BlockNodeKind+`' and t.from_id = any($1) and t.kind = '`+graphstore.TupleConstrainedBy+`'
				and t.to_kind <> '`+graphstore.KindValue+`'`)+`
		) held
		where held.asserted
	`, addresses)
	if err != nil {
		return nil, err
	}
	constrained, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (graphstore.Tuple, error) {
		tuple := graphstore.Tuple{Kind: graphstore.TupleConstrainedBy, At: at, Who: writer, Asserted: false}
		err := row.Scan(&tuple.FromKind, &tuple.FromID, &tuple.ToKind, &tuple.ToID)
		return tuple, err
	})
	if err != nil {
		return nil, err
	}
	retracted := make([]graphstore.UncausedTuple, 0, len(prevented)+len(constrained))
	perBlock := map[string]int{}
	for _, tuple := range prevented {
		retracted = append(retracted, graphstore.UncausedTuple{Tuple: tuple})
		perBlock[tuple.ToID]++
	}
	for _, tuple := range constrained {
		retracted = append(retracted, graphstore.UncausedTuple{Tuple: tuple})
		perBlock[tuple.FromID]++
	}
	for _, block := range slices.Sorted(maps.Keys(perBlock)) {
		slog.Info("dispatch: a block the version no longer holds takes its pointers with it",
			"block", block, "retracted", perBlock[block])
	}
	return retracted, nil
}

// DocumentWindowKind is the embeddings kind of a window of a document's blocks, as
// ItemKind.WindowKind is of a comment's, a message's or an ask's.
const DocumentWindowKind = "document_block"

// windowKinds is every kind an embeddings window row is written under. A statement naming them
// as literals is one the planner can prove embeddings_window_owner's partial predicate for,
// whatever a parameter holds (writeWindows).
const windowKinds = `('document_block', 'comment_block', 'message_block', 'ask_part')`

// BlockEmbeddingStates is where meaning search stands on each block of the owner whose windows
// are kind kind under ownerKey: model.BlockEmbedded for a block every window covering it holds
// a vector of its current text for, model.BlockEmbeddingPending while one of them waits for its
// vector. A block no window covers is in no entry of the map, which a reader of one block reads
// as model.BlockNotEmbedded - a block the indexer puts in no window, and every block of an owner
// whose windows are not written yet. The read names the window kinds as literals beside the
// parameter, as every read and write of a window does (writeWindows), so the planner can prove
// embeddings_window_owner's partial predicate and reach the owner's rows through that index.
//
// It lives here, with the writes of the rows it reads, so the list of window kinds is held in
// one place: this package's windowKinds, migration 20261102233630's index predicate and the
// column the two of them name.
func BlockEmbeddingStates(ctx context.Context, q Queryer, kind, ownerKey string) (map[string]string, error) {
	rows, err := q.Query(ctx, `
		select covered.block_id, bool_and(e.embedded_hash is not distinct from e.content_hash)
		from embeddings e cross join unnest(e.covers) as covered(block_id)
		where e.kind = $1 and e.kind in `+windowKinds+`
		  and split_part(e.id, '#', 1) = $2
		group by covered.block_id
	`, kind, ownerKey)
	if err != nil {
		return nil, fmt.Errorf("read the embedding state of the blocks of %s %s: %w", kind, ownerKey, err)
	}
	defer rows.Close()
	states := map[string]string{}
	for rows.Next() {
		var blockID string
		var current bool
		if err := rows.Scan(&blockID, &current); err != nil {
			return nil, fmt.Errorf("scan the embedding state of a block of %s %s: %w", kind, ownerKey, err)
		}
		states[blockID] = model.BlockEmbeddingPending
		if current {
			states[blockID] = model.BlockEmbedded
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the embedding state of the blocks of %s %s: %w", kind, ownerKey, err)
	}
	return states, nil
}

// WriteDocumentWindows writes the windows of document artifactID's blocks in the caller's
// transaction, which holds the document's owner, and stamps the state those blocks are in
// beside them, so the window arm can tell these windows from ones an older build left behind:
// blocks are the document's blocks in document order, each parent before its children, and
// markdown the text they index into. The owner key of a document's windows is the room it is
// edited in (DocumentWindowOwner): its issue's tree artifact once it is converted into its
// issue's room, which keys every document of the room, so the write deletes only the windows
// whose first block is the document's or is in no fragment of the room (documentWindowsDropped),
// and the document itself where it is its own room. The context of each window is the document's
// own title and the heading above the window (itemblocks.DocumentWindows).
//
// It is the one writer of a document's windows. A version write records the blocks and marks
// the document owed the windows that stand for them, which the block-index tick's window arm
// writes (BlockIndexing.Queue, docs.windowDocument), windowing the stored latest version's
// markdown - the text the version write indexed - a tick behind that write.
//
// The owner and the title the windows embed are read first, and the one statement that writes
// them is the only one after them, so the transaction takes its transaction id at that statement
// and holds it only to the commit.
func WriteDocumentWindows(ctx context.Context, tx pgx.Tx, artifactID, markdown string, blocks []model.ArtifactBlock) error {
	owner, err := DocumentWindowOwner(ctx, tx, artifactID)
	if err != nil {
		return err
	}
	title, err := documentTitle(ctx, tx, artifactID)
	if err != nil {
		return err
	}
	nodes, _, _ := uniqueBlocks(blocks)
	// The state is read from document_block_versions inside the statement, which also clears the
	// marker that listed the document, so the clear and the windows it stands for cannot come
	// apart (DocumentsOwedWindows).
	return writeWindows(ctx, tx, DocumentWindowKind, owner,
		itemblocks.DocumentWindows(title, documentBlockTexts(markdown, nodes)), blockState{
			query: `update document_block_versions v set windows_owed_at = null
			          where v.artifact_id = $5::uuid returning ` + documentBlockState + ` as block_state`,
			args:    []any{artifactID},
			dropped: documentWindowsDropped,
		})
}

// documentTitle is the name document artifactID's artifacts row holds, which the windows of its
// blocks embed: the one reader of it is the window arm's write above, whose document is stored.
func documentTitle(ctx context.Context, q Queryer, artifactID string) (string, error) {
	var title string
	if err := q.QueryRow(ctx, `select name from artifacts where id = $1::uuid`, artifactID).Scan(&title); err != nil {
		return "", fmt.Errorf("read the title of %s for its windows: %w", artifactID, err)
	}
	return title, nil
}

// documentBlockTexts is each block of nodes holding text of its own, in document order, with
// that text (markdown[From:To]): a block inside another is left out, since the window of its
// parent already holds its text and no window should embed one passage twice. nodes are in
// document order, each parent before its children, so a block starting before the last kept one
// ended is inside it.
func documentBlockTexts(markdown string, nodes []model.ArtifactBlock) []itemblocks.WindowBlock {
	blocks := make([]itemblocks.WindowBlock, 0, len(nodes))
	end := 0
	for _, node := range nodes {
		if node.To <= node.From || node.From < end {
			continue
		}
		end = node.To
		blocks = append(blocks, itemblocks.WindowBlock{ID: node.ID, Type: node.Type, Text: markdown[node.From:node.To]})
	}
	return blocks
}

// blockState is how one write of an owner's windows (writeWindows) tells that write what state
// the blocks those windows cover are in: one query returning a single block_state column, which
// the write runs as a common table expression and stamps beside the windows. Both owners'
// queries are updates: an item's clears the marker that listed it on the kind's state row and
// returns the state that row holds (clearItemWindowsOwed), and a document's clears the same
// marker on document_block_versions and returns the version its blocks are indexed at
// (WriteDocumentWindows). The query numbers its parameters from $5 and passes their values in
// args, since writeWindows holds $1 to $4.
//
// dropped is the condition, over the embeddings row the write's delete reads and written as
// `and …`, that a window of the owner the new set does not name meets to be deleted, empty for a
// write that deletes every such window: a document's, whose owner, an issue room's tree artifact,
// keys the windows of every document of the room (documentWindowsDropped). It reads the
// parameters the query reads.
type blockState struct {
	query   string
	args    []any
	dropped string
}

// writeWindows writes windows as the windows of owner ownerKey, of kind kind, in the caller's
// transaction: each takes the id "<owner key>#<its id suffix>" through
// embeddings_enqueue_window, which rewrites a row only where the hash of its text moved, and the
// owner's rows of that kind the new set does not name are deleted. Rewriting an owner's windows
// when none of them changed therefore leaves every row's vector, attempts and wait as they
// stand, and an owner left with no block loses every window it had.
//
// The whole write is one statement, so a document of fifty windows costs one round trip, and the
// marker that listed the owner is settled in that same round trip, an item's and a document's
// alike (clearItemWindowsOwed, WriteDocumentWindows). It is the last statement of the
// transaction that sends it, and everything it writes was decided before it, so the transaction
// takes its transaction id here and holds it only to the commit (WriteDocumentWindows,
// WriteItemWindows). The windows go through jsonb_to_recordset in the top-level select; the
// state query, the stamp of what it returns and the delete are common table expressions, and
// Postgres runs a data-modifying one exactly once and to completion whether or not the select
// reads it. The delete cannot race the enqueue: it names only ids the new set does not hold, and
// the enqueue writes only ids it does.
//
// The delete names the kind twice, as the parameter and in the literal list of every window
// kind, because a generic plan knows nothing of what the parameter holds: the literal list is
// what lets the planner prove embeddings_window_owner's partial predicate and read the owner's
// rows through that index.
func writeWindows(ctx context.Context, tx pgx.Tx, kind, ownerKey string, windows []itemblocks.Window, state blockState) error {
	statement, args, err := windowsStatement(kind, ownerKey, windows, state)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, statement, args...); err != nil {
		return fmt.Errorf("write the windows of %s %s: %w", kind, ownerKey, err)
	}
	return nil
}

// queueWindows is writeWindows' one statement queued on a write phase's batch, for a writer that
// decides the windows before it writes anything and sends its writes as one batch: the delete of
// a post, whose item is left with no block and so with no window (TombstoneItem).
func queueWindows(b *store.WriteBatch, kind, ownerKey string, windows []itemblocks.Window, state blockState) error {
	statement, args, err := windowsStatement(kind, ownerKey, windows, state)
	if err != nil {
		return err
	}
	b.Queue(fmt.Sprintf("write the windows of %s %s", kind, ownerKey), statement, args...)
	return nil
}

// windowsStatement is the one statement writeWindows sends and queueWindows queues, with its
// arguments.
func windowsStatement(kind, ownerKey string, windows []itemblocks.Window, state blockState) (string, []any, error) {
	type row struct {
		Suffix string   `json:"suffix"`
		Text   string   `json:"text"`
		Covers []string `json:"covers"`
	}
	rows := make([]row, len(windows))
	ids := make([]string, len(windows))
	for i, window := range windows {
		rows[i] = row{Suffix: window.IDSuffix, Text: window.Text, Covers: window.Covers}
		ids[i] = ownerKey + "#" + window.IDSuffix
	}
	payload, err := json.Marshal(rows)
	if err != nil {
		return "", nil, fmt.Errorf("encode the windows of %s %s: %w", kind, ownerKey, err)
	}
	return `
		with state as (
			` + state.query + `
		),
		stamped as (
			insert into embedding_window_states (kind, owner_key, block_state, written_at)
			select $1::text, $2::text, state.block_state, now() from state
			on conflict (kind, owner_key) do update
			  set block_state = excluded.block_state, written_at = excluded.written_at
		),
		dropped as (
			delete from embeddings
			where kind = $1 and kind in ` + windowKinds + `
			  and split_part(id, '#', 1) = $2 and id <> all($4::text[])` + state.dropped + `
		)
		select embeddings_enqueue_window($1, $2 || '#' || w.suffix, w.text, w.covers)
		from jsonb_to_recordset($3::jsonb) as w (suffix text, text text, covers text[])
	`, append([]any{kind, ownerKey, payload, ids}, state.args...), nil
}

// documentBlockState is the state of a document's blocks that its windows are stamped with
// (WriteDocumentWindows): the version they are indexed at, over document_block_versions v. The
// state is read from document_block_versions in the statement that writes the windows, so what
// is stamped is the version the blocks those windows cover are indexed at, and a document owed
// its first block index holds a null version, which stamps as the empty string.
//
// Nothing in the server reads the stamp back: windows_owed_at says which documents are owed
// their windows (DocumentsOwedWindows), and that marker replaced the comparison this stamp
// served. Two readers of it are left, which is why the write still makes it. Migration
// 20261108070100 reads it once, to mark every document whose windows were already stale when
// the column arrived, and the release before this one decides owedness from it, so a rollback
// finds the stamp it has always read.
const documentBlockState = `coalesce(v.version::text, '')`

// DocumentsOwedWindows lists at most limit documents whose blocks the index holds - a document
// can hold none, and such a one is listed too - and whose windows do not stand for those
// blocks, in artifact id order past after (empty: from the start), for the block-index tick's
// window arm. A document written before meaning search moved to windows has blocks and no
// window; one whose version write failed after its blocks were written has the same; and every
// document written since this arm last windowed it is one too, because a version write records
// the blocks and leaves the windows here (BlockIndexing.Queue, INFRA-988).
//
// The read is of the marker, windows_owed_at (migration 20261108070000), which is the
// question written down rather than asked of state, as the items' is (ItemsOwedWindows): the
// version write and 0122's trigger set it, rebuild-refs reaches it through the re-index, and
// migration 20261108070100 set it once for every document whose windows were already stale
// when the column arrived. The one statement that writes a document's windows clears it,
// beside the stamp of the version those windows were written from (WriteDocumentWindows,
// writeWindows), so the clear and the windows cannot come apart; that statement's own
// `dropped` is the only delete of a window row in the server, and a route that removes one
// otherwise marks its owner here, or this read will not see it.
//
// The reading this replaced decided owedness from state - whether a stamp stood at the
// version the blocks are indexed at, and whether a window row existed at all - which is two
// index probes for every row of document_block_versions: 112-122 ms a pass with 67,496 buffer
// hits over the load-test deployment's 9,881 documents, for an answer of no rows, and a tick
// runs it every 2 s while the arm has a full batch (INFRA-988).
func DocumentsOwedWindows(ctx context.Context, q Queryer, after string, limit int) ([]string, error) {
	rows, err := q.Query(ctx, documentsOwedWindowsQuery, uuidAfter(after), limit)
	if err != nil {
		return nil, fmt.Errorf("list the documents whose windows do not stand for their blocks: %w", err)
	}
	documents, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("list the documents whose windows do not stand for their blocks: %w", err)
	}
	return documents, nil
}

// documentsOwedWindowsQuery is the statement DocumentsOwedWindows sends: $1 is the id its
// batch starts after and $2 the most ids it answers. The tests read the statement from here
// rather than from a copy of it (document_windows_owed_test.go).
//
// The driving rows are the marked rows of document_block_versions, read through its partial
// index on windows_owed_at, whose key is the artifact id: the index holds the owed rows
// alone, in the order the batch pages by, so the keyset cursor and the limit are both the
// index's own and the read costs what the arm has to do rather than what the deployment has
// stored.
//
// Two kinds of marked row are this arm's, and the lateral join is what tells them from the
// rows that are not:
//
//   - a document the block index has reached, whose row carries the version its blocks are
//     indexed at. A version holding no block is one of them: an issue created with no spec
//     carries a spec.md whose markdown is empty, so the index holds no row of
//     document_blocks for it to be found by. The arm windows such a document to no window,
//     which deletes whatever windows a version that did hold blocks left behind and clears
//     the marker in the one statement (WriteDocumentWindows, writeWindows). The semi-join
//     this replaced dropped it, and since that statement is the only one that clears the
//     marker, the row stayed marked for ever: an entry in the partial index that every later
//     pass paid a probe for and no pass could ever remove (INFRA-988).
//   - a document owed a block index (version null) that holds the blocks of an earlier
//     version. Its stored version's windows are this arm's to write while the owed arm
//     cannot index it, which is what a document whose live text stays ahead of its latest
//     version needs (docs.windowDocument). Its blocks are the rows of the room it is edited
//     in whose block it holds (DocumentRoomBlocksSQL): an issue born converted keys its
//     documents' rows under its tree artifact, and a document of such a room is owed its
//     index while another fragment holds an edit no version holds yet.
//
// A marked row with neither - a document an older build wrote, owed a block index and holding
// no block yet - is not this arm's, as the items' is not: windows under no block are not its
// to write, the owed-document arm indexes it, and that index marks it here again.
//
// The blocks are read through a lateral join rather than as `v.version is not null or exists
// (…)`: an EXISTS under an OR is a correlated subplan the planner runs per marked row, which
// the plan test refuses, while the join is one probe for each marked row read, as the
// semi-join before it was, and the inner limit stops it at the first block row. For the same
// reason the room and each row's document are read as joins on document_rooms and
// issue_room_blocks, the rows DocumentRoomSQL and BlockDocumentSQL read, rather than through
// those scalar probes, as storedBlocks reads the room's rows: a scalar probe is a subplan. The
// join runs only for a row owed its block index, the one row it decides: a document the index
// has reached is listed whatever it holds, and an empty document of a room of thousands of
// blocks is no scan of that room each time it is listed.
const documentsOwedWindowsQuery = `
	select v.artifact_id::text from document_block_versions v
	left join document_rooms moved on moved.artifact_id = v.artifact_id
	left join lateral (
		select 1 from document_blocks b
		left join issue_room_blocks room_block on room_block.tree_artifact_id = b.artifact_id
		  and block_id_key(room_block.block_id) = block_id_key(b.block_id) and room_block.block_id = b.block_id
		where v.version is null and b.artifact_id = coalesce(moved.room, v.artifact_id)
		  and coalesce(room_block.document_id, b.artifact_id) = v.artifact_id
		limit 1
	) held (block) on true
	where v.windows_owed_at is not null
	  and v.artifact_id > $1::uuid
	  and (v.version is not null or held.block is not null)
	order by v.artifact_id limit $2`

// uuidAfter is a keyset cursor over uuids: the id a batch starts after, or the nil uuid, which
// is below every uuid a row holds, when a batch starts from the beginning.
func uuidAfter(after string) string {
	if after == "" {
		return "00000000-0000-0000-0000-000000000000"
	}
	return after
}

// citedBlocks is, of the document's blocks named by ids, each that a source cites (a refs or a
// block_refs row points at it), as a block target at its node, read through q before the indexing
// is written. A citation keeps the address it was written at, so a block of an issue room, whose
// node is the room tree's, is cited at its node or at the same block id under any document of the
// room (document_rooms), each of which resolves to that node (ResolvedBlockSQL,
// block_room_address), as documentRoomBlockAddresses reads them for the read of a document; a
// document that is its own room holds no such address.
func citedBlocks(ctx context.Context, q Queryer, artifactID string, ids []string) ([]Target, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	cited, err := scanTargets(q.Query(ctx, `
		select 'block', b.node_id
		from (
			select a.ref_key || '#' || t.id as node_id,
			       array[a.ref_key || '#' || t.id] || array(
			         select joined_document.ref_key || '#' || t.id from document_rooms joined
			         join artifacts joined_document on joined_document.id = joined.artifact_id
			         where joined.room = $1::uuid) as addresses
			from unnest($2::text[]) as t (id)
			cross join (select ref_key from artifacts where id = $1::uuid) a
		) b
		where exists (select 1 from refs r where r.to_kind = 'block' and r.to_id = any(b.addresses))
		   or exists (select 1 from block_refs r where r.to_kind = 'block' and r.to_id = any(b.addresses))
	`, artifactID, ids))
	if err != nil {
		return nil, fmt.Errorf("read the cited blocks of %s: %w", artifactID, err)
	}
	return cited, nil
}

// blockSumHex is a block text's sha256 as document_blocks stores it: the hex of the 32 bytes,
// held in an array so the pass over a document's blocks can hash each one into a buffer of its
// own and compare it against the stored sum with no string made (string(sum[:]) == stored
// allocates nothing).
type blockSumHex [2 * sha256.Size]byte

// blockTextSum writes the sha256 (hex) of a block's text (markdown[From:To]) into sum.
func blockTextSum(sum *blockSumHex, body string) {
	hashed := sha256.Sum256([]byte(body))
	hex.Encode(sum[:], hashed[:])
}

// BlockTextSum is the sha256 (hex) of a block's text (markdown[From:To]), as document_blocks
// stores it in text_sha256.
func BlockTextSum(body string) string {
	var sum blockSumHex
	blockTextSum(&sum, body)
	return string(sum[:])
}

// storedBlock is what the document's index already holds of one block: the sha256 of its text,
// when its document_blocks row was written, which is the time a membership of the document is
// dated from (TupleInDocument), the node that row names, "" for a row that names none, and the
// supertag that node carries, "" where it names none or the store holds no such node, the
// document the block is a block of (BlockDocumentSQL), whose version a block the version drops
// records, and whether that document is a retired document of the issue room whose tree artifact
// keys the row, whose blocks a pass over the room keeps as they stand (BlockIndexing.retained).
type storedBlock struct {
	sum      string
	joined   time.Time
	node     string
	supertag string
	document string
	retired  bool
}

// namedNode is the node a block's row names once a pass has written or placed it: its address
// wherever that fits the store's key, since the parents' projection names it on every placed
// row, and none otherwise.
func namedNode(refKey, blockID string, fits bool) string {
	if !fits {
		return ""
	}
	return blockNodeAddress(refKey, blockID)
}

// NodeOf is the node the document_blocks row of block blockID names in the index as it stands -
// as ReadBlockIndex read it, or as a planned indexing leaves it (BlockIndexing.Held) before its
// write phase has run - and false where the index holds no row of that block or its row names no
// node.
func (i BlockIndex) NodeOf(blockID string) (string, bool) {
	block, held := i.blocks[blockID]
	return block.node, held && block.node != ""
}

// heldSupertags is the supertag the store holds for each of ids, blocks of the document refKey
// names, by id: a block a version deletes keeps the supertag its node carries, since no block of
// that version is left to say its type. An id the store holds no node of is absent. The read is
// the plan's, in its write's read phase.
func heldSupertags(ctx context.Context, q Queryer, refKey string, ids []string) (map[string]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := q.Query(ctx, `
		select substr(n.id, length($2) + 2), n.supertag
		from graph_nodes n
		where n.kind = $1 and n.id = any(select $2 || '#' || id from unnest($3::text[]) as deleted (id))
	`, BlockNodeKind, refKey, ids)
	if err != nil {
		return nil, fmt.Errorf("read the supertags of the blocks of %s a version deletes: %w", refKey, err)
	}
	defer rows.Close()
	held := make(map[string]string, len(ids))
	for rows.Next() {
		var id, supertag string
		if err := rows.Scan(&id, &supertag); err != nil {
			return nil, fmt.Errorf("scan the supertag of a block of %s: %w", refKey, err)
		}
		held[id] = supertag
	}
	return held, rows.Err()
}

// storedBlocks is each block the document's index holds, by id, and, by id, the version its text
// last changed in for each block whose changed version another renderer version stored or none
// stamped (pmdoc.OtherRenderer), which a version the document no longer has counts as.
//
// It reads the node columns, the row's created_at and the renderer stamp of its changed version
// beside the digest, in the one read of the document's rows the plan takes, because the pass
// writes a node and a membership for every row that names none (PlanBlocks) and a second read of
// the same rows would cost a scan of the document for every version. The document of each block
// is BlockDocumentSQL's, read as a join on the room's rows (none for a document that is its own
// room) rather than a probe a row, and so is whether that document is a retired document of the
// room (storedBlock.retired); the changed version is a version of that document, whose row holds
// the stamp. The supertag of the node a row names is read by a probe of graph_nodes' primary key a
// row, behind an offset 0 the planner cannot flatten into a join over the whole table, because the
// pass restates the node of every block of the version whose supertag is not the one its type
// gives (PlanBlocks).
func storedBlocks(ctx context.Context, q Queryer, artifactID string) (map[string]storedBlock, map[string]int, error) {
	rows, err := q.Query(ctx, `
		select d.block_id, d.text_sha256, d.created_at,
		       case when d.node_kind is not null then coalesce(d.node_address, '') else '' end,
		       coalesce(node.supertag, ''),
		       coalesce(r.document_id, d.artifact_id)::text, retired.id is not null,
		       d.changed_version, v.renderer_version
		from document_blocks d
		left join (select block_id, document_id from issue_room_blocks where tree_artifact_id = $1) r
		       on r.block_id = d.block_id
		left join artifacts retired on retired.id = r.document_id and retired.id <> $1 and retired.retired_at is not null
		left join artifact_versions v on v.artifact_id = coalesce(r.document_id, d.artifact_id) and v.number = d.changed_version
		left join lateral (
			select g.supertag from graph_nodes g
			where g.kind = d.node_kind and g.id = d.node_address
			offset 0
		) node on true
		where d.artifact_id = $1
	`, artifactID)
	if err != nil {
		return nil, nil, fmt.Errorf("read the blocks of %s: %w", artifactID, err)
	}
	defer rows.Close()
	stored := make(map[string]storedBlock)
	var respellable map[string]int
	for rows.Next() {
		var id string
		var held storedBlock
		var changedVersion int
		var stamp *int
		if err := rows.Scan(&id, &held.sum, &held.joined, &held.node, &held.supertag, &held.document, &held.retired, &changedVersion, &stamp); err != nil {
			return nil, nil, fmt.Errorf("scan a block of %s: %w", artifactID, err)
		}
		stored[id] = held
		if pmdoc.OtherRenderer(stamp) {
			if respellable == nil {
				respellable = make(map[string]int)
			}
			respellable[id] = changedVersion
		}
	}
	return stored, respellable, rows.Err()
}

// blockPair is one link a block holds: the block's id, the edge's kind and its target, and the
// block types it points at (nil: any). nodeKind and nodeAddress name the node the link hangs on
// where it is not the block's own: an ask block's shaped hangs on the ask's node, whose row is
// marked ('ask', <ask id>) (TypedFacts.pairs).
type blockPair struct {
	blockID     string
	kind        string
	target      Target
	toTypes     []string
	nodeKind    string
	nodeAddress string
}

// from is the node the pair's tuple is stated out of: the node it names of its own (an ask's, for
// an ask block's shaped), and the block's own otherwise, whose address is its document's ref_key
// and its id whether or not the row names it.
func (p blockPair) from(refKey string) (kind, address string) {
	if p.nodeKind != "" && p.nodeKind != BlockNodeKind {
		return p.nodeKind, p.nodeAddress
	}
	return BlockNodeKind, blockNodeAddress(refKey, p.blockID)
}

// blockPairKey is how block_refs keys a pair: all of it but the block types it points at, which
// a surviving pair takes from the version being indexed, and the node it hangs on, which the
// insert writes and a pair that survives on another node moves (planBlockRefs).
type blockPairKey struct {
	blockID string
	kind    string
	target  Target
}

// key is the pair as block_refs keys it.
func (p blockPair) key() blockPairKey {
	return blockPairKey{blockID: p.blockID, kind: p.kind, target: p.target}
}

// pairScope is the rows of a document's block_refs one reconcile may delete (planBlockRefs,
// queueBlockRefs): those of the tuple kinds kinds held by the blocks blocks, and every row of any
// kind held by the blocks whole, the typed blocks a deferred version removes (PlanDecisionNodes),
// each list sorted.
type pairScope struct {
	kinds, blocks, whole []string
}

// reaches says the reconcile may delete pair's row: every row where there is no scope, and where
// there is one, a row of one of its kinds held by one of its blocks, or any row of a whole block.
func (s *pairScope) reaches(pair blockPair) bool {
	if s == nil {
		return true
	}
	if _, whole := slices.BinarySearch(s.whole, pair.blockID); whole {
		return true
	}
	_, held := slices.BinarySearch(s.blocks, pair.blockID)
	return held && slices.Contains(s.kinds, pair.kind)
}

// heldTarget is a target as one block holds it.
type heldTarget struct {
	blockID string
	target  Target
}

// blockPairs is each distinct link the pointers of nodes and the mentions in located write: a
// pointer from its typed block (pointerPairs), a mention from the innermost of nodes holding its
// start. A mention no block holds, and an external URL, write none; nor does a block's mention of
// a target its own pointers account for (PlanBlocks).
func blockPairs(nodes []model.ArtifactBlock, located []text.Located, pointers []Pointer) []blockPair {
	pairs, pointed := pointerPairs(nodes, pointers)
	pairs = slices.Grow(pairs, len(located))

	starts := make([]int, len(located))
	for i, mention := range located {
		starts[i] = mention.Offset
	}
	holders := innermostBlocks(nodes, starts)
	mentions := make([]heldTarget, 0, len(located))
	counts := make(map[heldTarget]int, len(located))
	for i, mention := range located {
		if holders[i] < 0 {
			continue
		}
		target, ok := TargetOf(mention.Ref)
		if !ok {
			continue
		}
		held := heldTarget{blockID: nodes[holders[i]].ID, target: target}
		if counts[held] == 0 {
			mentions = append(mentions, held)
		}
		counts[held]++
	}
	for _, held := range mentions {
		if counts[held] > pointed[held] {
			pairs = append(pairs, blockPair{blockID: held.blockID, kind: "mentions", target: held.target})
		}
	}
	return pairs
}

// pointerPairs is each distinct link the pointers of nodes write, from its typed block, and the
// number of each block's pointer values naming each target. A pinned and an unpinned value of one
// attribute name one target, and write one pair. No attribute is named mentions (pmdoc), so no
// pointer's pair is a mention's. A pointer writes a pair only from the node uniqueBlocks keeps for
// its block id, the first holder of the id with an offset, and only when that node is the typed
// block holding it (its type is the pointer's BlockType). pmdoc.BlockPointers already lists none
// of a typed block repeating the id of a typed block before it. The type check keeps a typed
// block's pointers off a first holder of its id that is not typed (a paragraph, a heading). It is
// not what makes the two paths agree: a typed block's id reads back from the markdown as the tree
// holds it (pmdoc.TypedBlockIDAttribute), so the version write, indexing the tree, and the
// reconciliation, reading the markdown back, list the same pointers under that id and resolve it
// to the same node of the same render, and without the check both would write the same wrong edge
// from that holder. A list item has an offset, so a list item holding an id first is the id's
// node. It is no typed block holding a pointer, so a typed block after it that repeats the id
// writes none, as one after a paragraph does. A holder with no offset (a table cell) is never a
// node, so a typed block after it is the id's node and keeps its pointers. One whose block is no
// node (an id past the bound, say) or not its own writes nothing and is charged against no mention.
func pointerPairs(nodes []model.ArtifactBlock, pointers []Pointer) ([]blockPair, map[heldTarget]int) {
	if len(pointers) == 0 {
		return nil, nil
	}
	type pointerKey struct {
		held heldTarget
		kind string
	}
	written := make(map[pointerKey]struct{}, len(pointers))
	nodeTypes := make(map[string]string, len(nodes))
	for _, node := range nodes {
		nodeTypes[node.ID] = node.Type
	}
	pairs := make([]blockPair, 0, len(pointers))
	pointed := make(map[heldTarget]int, len(pointers))
	for _, pointer := range pointers {
		if typ, ok := nodeTypes[pointer.Block]; !ok || typ != pointer.BlockType {
			continue
		}
		target, _ := TargetOf(pointer.Target)
		held := heldTarget{blockID: pointer.Block, target: target}
		pointed[held]++
		key := pointerKey{held: held, kind: pointer.Kind}
		if _, dup := written[key]; dup {
			continue
		}
		written[key] = struct{}{}
		pairs = append(pairs, blockPair{blockID: pointer.Block, kind: pointer.Kind, target: target, toTypes: pointer.Targets})
	}
	return pairs, pointed
}

// resolvePointerPairs rewrites, in place, the far end of each of pairs a pointer holds - every
// pair but a block's mention whose target is a block: a `block[]` attribute's, a decision's or an
// ask block's tie (pointerPairs) - to the node it resolves to now (ResolvedBlockSQL), read through
// q in one statement, in the plan's read phase, before the pairs are reconciled (planBlockRefs):
// the row, its tuple and the targets the plan answers name that node, and so does every read the
// plan makes of the pairs after it, such as the links it retracts beside them (unheldLinks). A far
// end that resolves to no block takes the address the resolution's two steps name without asking
// for a row (UnheldBlockSQL) where that address is one of written, the node ids of the blocks this
// same pass writes, whose rows its write phase inserts after this read - a block of another
// fragment of the issue room the pass indexes - or where a block_refs row of document artifactID
// holds it, its block deleted: so the pair keeps the far end its row and its tuple already name, and its
// row stays, as the row of a pointer at a deleted block of a document that is its own room stays;
// a later version that drops the pointer retracts that tuple. Any other far end stays as written.
// Two pointers of one block and one kind that name two addresses of one node are one link once
// resolved, so it answers pairs with each such repeat after the first left out, a slice of its own
// where there is one and pairs itself where there is none.
func resolvePointerPairs(ctx context.Context, q Queryer, artifactID string, written []string, pairs []blockPair) ([]blockPair, error) {
	pointer := func(pair blockPair) bool { return pair.kind != "mentions" && pair.target.Kind == BlockNodeKind }
	var addresses []string
	for _, pair := range pairs {
		if pointer(pair) {
			addresses = append(addresses, pair.target.ID)
		}
	}
	if len(addresses) == 0 {
		return pairs, nil
	}
	resolved, err := resolvePassFarEnds(ctx, q, artifactID, addresses, written)
	if err != nil || len(resolved) == 0 {
		return pairs, err
	}
	for index, pair := range pairs {
		if node, moved := resolved[pair.target.ID]; moved && pointer(pair) {
			pairs[index].target.ID = node
		}
	}
	seen := make(map[blockPairKey]struct{}, len(pairs))
	var unique []blockPair
	for index, pair := range pairs {
		key := pair.key()
		if _, repeated := seen[key]; repeated {
			if unique == nil {
				unique = slices.Clone(pairs[:index])
			}
			continue
		}
		seen[key] = struct{}{}
		if unique != nil {
			unique = append(unique, pair)
		}
	}
	if unique == nil {
		return pairs, nil
	}
	return unique, nil
}

// resolvePassFarEnds resolves, through q in one statement, each of addresses, a block far end a
// pointer of document artifactID names, to the node it resolves to now (ResolvedBlockSQL), and,
// where that is none, to the address the resolution's two steps name without asking for a row
// (UnheldBlockSQL) where that address is one of written - a block the same pass writes - or one a
// block_refs row of artifactID holds (resolvePointerPairs says why). An address of a document the
// reading transaction creates and joins to an issue room, whose artifacts and document_rooms rows
// no read before its write phase sees, takes that room's step from the transaction
// (pendingRoomFarEnds): its block in the room, where that is one of written. It answers each
// address that resolves to another node than itself, by address.
func resolvePassFarEnds(ctx context.Context, q Queryer, artifactID string, addresses, written []string) (map[string]string, error) {
	resolved, addresses, err := pendingRoomFarEnds(ctx, q, addresses, written)
	if err != nil || len(addresses) == 0 {
		return resolved, err
	}
	rows, err := q.Query(ctx, `
		select p.address, resolved.node_id
		from (select distinct unnest($2::text[]) as address) p
		cross join lateral (
			select coalesce(`+ResolvedBlockSQL("p.address")+`, (
				select named.address from (select `+UnheldBlockSQL("p.address")+` as address) named
				where named.address = any($3::text[]) or exists (select 1 from block_refs held
					where held.artifact_id = $1::uuid and held.to_kind = '`+BlockNodeKind+`' and held.to_id = named.address)
			)) as node_id
		) resolved
		where resolved.node_id <> p.address
	`, artifactID, addresses, written)
	if err != nil {
		return nil, fmt.Errorf("resolve the far ends of the pointers of %s: %w", artifactID, err)
	}
	defer rows.Close()
	for rows.Next() {
		var address, node string
		if err := rows.Scan(&address, &node); err != nil {
			return nil, fmt.Errorf("scan a resolved far end of %s: %w", artifactID, err)
		}
		resolved[address] = node
	}
	return resolved, rows.Err()
}

// pendingRoomFarEnds resolves each of addresses whose document the reading transaction creates and
// joins to an issue room (Pending.PendingRoom) to that room's block of the same id, `<room
// ref_key>#<block id>`, where that is one of written: the room step the store takes once those
// rows stand (UnheldBlockSQL), taken from the transaction, so a pointer of a document's first
// version at a block the same pass writes in the room names the room's node, as the same pointer
// of a later version does. The room's ref_key is the transaction's where it creates the room too
// (an issue born converted's tree), else the stored row's (the tree of a converted issue that an
// upload's Verification document or plan joins), read through q in one statement. It answers
// those it resolved, by address, and every other address, which the store resolves.
func pendingRoomFarEnds(ctx context.Context, q Queryer, addresses, written []string) (map[string]string, []string, error) {
	resolved := make(map[string]string)
	pending := pendingOf(ctx)
	if pending == nil {
		return resolved, addresses, nil
	}
	joined := make(map[string]string)
	roomKeys := make(map[string]string)
	var unread []string
	for _, address := range addresses {
		document, _, cut := strings.Cut(address, "#")
		room, joins := pending.PendingRoom(document)
		if !cut || !joins {
			continue
		}
		joined[address] = room
		if created, pendingRoom := pending.PendingDocument(room); pendingRoom {
			roomKeys[room] = created.RefKey
		} else if !slices.Contains(unread, room) {
			unread = append(unread, room)
		}
	}
	if len(joined) == 0 {
		return resolved, addresses, nil
	}
	if len(unread) > 0 {
		rows, err := q.Query(ctx, `select id::text, ref_key from artifacts where id = any($1::uuid[])`, unread)
		if err != nil {
			return nil, nil, fmt.Errorf("read the ref_keys of the issue rooms %v: %w", unread, err)
		}
		defer rows.Close()
		for rows.Next() {
			var room, key string
			if err := rows.Scan(&room, &key); err != nil {
				return nil, nil, fmt.Errorf("scan the ref_key of an issue room: %w", err)
			}
			roomKeys[room] = key
		}
		if err := rows.Err(); err != nil {
			return nil, nil, fmt.Errorf("read the ref_keys of the issue rooms %v: %w", unread, err)
		}
	}
	writes := make(map[string]struct{}, len(written))
	for _, address := range written {
		writes[address] = struct{}{}
	}
	stored := make([]string, 0, len(addresses))
	for _, address := range addresses {
		if room, joins := joined[address]; joins {
			_, block, _ := strings.Cut(address, "#")
			if key := roomKeys[room]; key != "" {
				if _, wrote := writes[key+"#"+block]; wrote {
					resolved[address] = key + "#" + block
					continue
				}
			}
		}
		stored = append(stored, address)
	}
	return resolved, stored, nil
}

// BlockLinks lists each link from a block of document fromArtifact to a block of document
// toArtifact (artifact uuids; the two may be one document), newest first, then by the source
// block's node id, then by the target's. A link counts only while both of its blocks are current
// blocks of their documents: a deleted block's links drop out, and its stored mentions bring them
// back when a version holds its id again. A link's target is the node its far end resolves to
// (ResolvedBlockSQL: its moved_to tuples first, then its document's room, then itself, through
// movedTarget), which it names: so a citation written before its target moved still reaches it
// (R8), and a citation of a block of a document converted into its issue's room, written at the
// document's own address, reaches the room's node. A mention whose target's moves end at an ask
// whose block toArtifact holds (moved_to_reach, AsksOfDocumentSQL) - the heading of a section that
// cites an answered ask a person removed - counts as a link to that ask, which it names. A
// pointer's link counts only while the block it reaches has a type the pointer allows, tested as
// the arm of graph_edges that answers the row tests it (blockRefListed), and a pointer reaches no
// ask. A link either of whose ends who may not see drops out the same way: the statement that
// reads the links, which reads the store where it resolves a far end, binds each end's predicate
// (VisibleKind), as every read of a block does, and the nodes read after it answer neither end.
//
// It reads no document's address and follows no document's move: it takes the two documents by
// uuid, and a document the pre-mortem conversion retired, whose address moved to a lens
// (api.ConvertPremortem), keeps its row, its uuid, its versions and its blocks, so the links to its
// blocks from a third document read as they did before it was retired. A document's blocks are
// the rows of the room it is edited in (DocumentRoomSQL) that are its own (BlockDocumentSQL): a
// converted document's are the rows of its fragment, which its issue room's tree artifact keys,
// with their links.
func BlockLinks(ctx context.Context, q Queryer, who model.Actor, fromArtifact, toArtifact string) ([]model.BlockLink, error) {
	rows, err := q.Query(ctx, `
		select r.kind, r.from_node, coalesce(t.node_id, k.id::text), case when t.node_id is null then '`+graphstore.KindAsk+`' else 'block' end,
		       r.created_at, r.created_version
		from block_refs r
		join document_blocks f on f.artifact_id = r.artifact_id and f.block_id = r.block_id
		cross join lateral (`+movedTarget(`r.to_id`)+`
		) target
		left join document_blocks t on t.node_id = target.node_id and t.artifact_id = `+DocumentRoomSQL("$2::uuid")+`
		  and `+BlockDocumentSQL("t")+` = $2::uuid
		left join asks k on k.id = case when target.ask_id is not null then target.ask_id::uuid end
		  and `+AsksOfDocumentSQL("k", "$2::uuid")+`
		where r.artifact_id = `+DocumentRoomSQL("$1::uuid")+` and r.to_kind = 'block'
		  and `+BlockDocumentSQL("f")+` = $1::uuid
		  and (t.node_id is not null and `+blockRefListed("r", "t.node_id")+` or k.id is not null and r.kind = 'mentions')
		  and `+VisibleKind(BlockNodeKind, "r.from_node", "$3")+`
		  and (t.node_id is null or `+VisibleKind(BlockNodeKind, "t.node_id", "$3")+`)
		  and (k.id is null or `+VisibleKind(string(ItemAsk), "k.id::text", "$3")+`)
		order by r.created_at desc, r.from_node, coalesce(t.node_id, k.id::text)`, fromArtifact, toArtifact, VisibilityArg(who))
	if err != nil {
		return nil, fmt.Errorf("list the block links from %s to %s: %w", fromArtifact, toArtifact, err)
	}
	type linkRow struct {
		kind, from, to, toKind string
		createdAt              time.Time
		createdVersion         int
	}
	var links []linkRow
	var ends []edgeRow
	for rows.Next() {
		var link linkRow
		if err := rows.Scan(&link.kind, &link.from, &link.to, &link.toKind, &link.createdAt, &link.createdVersion); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan a block link: %w", err)
		}
		links = append(links, link)
		ends = append(ends, edgeRow{otherKind: "block", otherID: link.from}, edgeRow{otherKind: link.toKind, otherID: link.to})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list the block links from %s to %s: %w", fromArtifact, toArtifact, err)
	}
	nodes, err := resolveNodes(ctx, q, who, true, ends)
	if err != nil {
		return nil, err
	}
	result := make([]model.BlockLink, 0, len(links))
	for _, link := range links {
		from, fromFound := nodes[[2]string{"block", link.from}]
		to, toFound := nodes[[2]string{link.toKind, link.to}]
		if !fromFound || !toFound {
			// A block deleted between the two reads: its links drop out as they would have.
			continue
		}
		result = append(result, model.BlockLink{
			Kind:           link.kind,
			From:           from.GraphNode,
			To:             to.GraphNode,
			CreatedAt:      link.createdAt,
			CreatedVersion: link.createdVersion,
		})
	}
	return result, nil
}

// BlockChange is when a block of a document last changed: the version its text last changed in
// and that version's time, as document_blocks records them (BlockIndexing.Queue).
type BlockChange struct {
	Version int
	At      time.Time
}

// BlockChanges is each current block of document artifactID, by block id, with when its text last
// changed. It is the document's own half of the out-of-date rule a decision record is read by: a
// decision's changed version and time are the greatest of its blocks', and BlockLinks gives the
// same facts for the blocks it links to. A document the block index has not reached has no rows
// and answers an empty map.
func BlockChanges(ctx context.Context, q Queryer, artifactID string) (map[string]BlockChange, error) {
	rows, err := q.Query(ctx, `
		select block_id, changed_version, changed_at from document_blocks where artifact_id = $1::uuid
	`, artifactID)
	if err != nil {
		return nil, fmt.Errorf("list the block changes of %s: %w", artifactID, err)
	}
	defer rows.Close()
	changes := map[string]BlockChange{}
	for rows.Next() {
		var blockID string
		var change BlockChange
		if err := rows.Scan(&blockID, &change.Version, &change.At); err != nil {
			return nil, fmt.Errorf("scan a block change of %s: %w", artifactID, err)
		}
		changes[blockID] = change
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list the block changes of %s: %w", artifactID, err)
	}
	return changes, nil
}

// DocumentAddress is where a document sits: the reference key its blocks' node ids are built
// from, the issue holding it, the parent of an issue this transaction creates too, and the
// dispatch:// address a citation of the document reports. A write hands the address over rather
// than a read of it taking those from the artifacts row, because a transaction creating the
// document has only queued that row: its own reads cannot see it, while the caller knows what
// the queued insert will write. It is what a read of the document's address answers before and
// after the write phase has run (docs.addressOfDocument), and what a citation of the document
// resolves to meanwhile (Pending.PendingArtifact).
type DocumentAddress struct {
	// RefKey is the document's reference key, its owner's key and its slug (migration 0071).
	RefKey string
	// IssueKey is the issue the document belongs to, "" for a project's or a session's document.
	IssueKey string
	// Ref is the document's own dispatch:// address, as ArtifactRef spells it from the row the
	// insert writes: the reference a Referenced by panel is opened on, which an event naming
	// the document as a changed target carries (model.ChangedReference.Ref).
	Ref string
	// PendingParent is the parent of IssueKey when this transaction creates that issue's row
	// too, "" when it does not or when the issue is a root. The ancestors walk starts there
	// while the issues row is only queued: no read of this transaction can see that row, so the
	// walk from the issue itself (IssueAncestors) would answer nothing, and the ancestors are
	// that parent followed by its own. A pending issue with no parent is a root, and both paths
	// answer the empty list.
	PendingParent string
	// Project is the project the document belongs to, an issue's or its own, and Session the
	// session whose conversation holds it, "" where it is a project's: the scope its block
	// nodes carry (IndexedVersion's Project and Parties), from the address this transaction
	// recorded for a row it is creating or from the stored row (docs' addressOfDocument).
	Project, Session string
	// Band is the band the document's blocks rank in (DocumentBand of the row the insert writes,
	// IndexedVersion's Band), zero where the writer does not say. Only an address this
	// transaction recorded for a row it is creating carries it; a stored row's version is read
	// with its band (ReadIndexedVersion).
	Band byte
}

// BlockTargets is where a write to document artifactID, which sits at address, lands and the
// target each of targets names, by node id (text.TargetID), as the block index would hold it once
// that write, leaving the blocks written (rendered, in document order, each parent before its
// children), is indexed, read through q: a block of that document as written's node for its id
// (uniqueBlocks: the first holder of a repeated id, and no id the index cannot key); any other
// block as the node its address resolves to (ResolvedBlockSQL: its moved_to tuples, then its
// document's room, then itself) is held, with the type that node has and the document holding it
// (BlockDocumentSQL: a converted document's, never its room's tree), with one query for every such
// target; an ask, a comment or a message, or a current block of one, as conversationTargets reads
// it, with its item's own issue and R2's test at the write's time; and an issue by its key. A
// target no row has is absent. The write lands in the document's own issue, or in none when a
// project holds it; that issue's ancestors are read only once a target turns out to sit in another
// document or in another issue's item (IssueAncestors, walked from DocumentAddress.PendingParent
// when the address names one), since a pointer inside the writing document - a plan task's
// `needs`, the common case - is held to nothing above it (pmdoc.CheckBlockPointers), and its
// descendants only once a target is an issue (IssueDescendants, read as a reader who proves no
// author reads it, since a rule of a write names no reader). This runs inside the write's
// transaction.
func BlockTargets(
	ctx context.Context,
	q Queryer,
	artifactID string,
	address DocumentAddress,
	written []model.ArtifactBlock,
	targets []text.Ref,
) (pmdoc.BlockWriting, map[string]pmdoc.BlockTarget, error) {
	writing := pmdoc.BlockWriting{Document: address.RefKey, Issue: address.IssueKey}
	nodes, _, _ := uniqueBlocks(written)
	own := make(map[string]string, len(nodes))
	for _, node := range nodes {
		own[node.ID] = node.Type
	}
	found := make(map[string]pmdoc.BlockTarget, len(targets))
	var elsewhere, issues []string
	var items []text.Ref
	for _, target := range targets {
		nodeID := text.TargetID(target)
		switch {
		case target.Kind == "issue":
			issues = append(issues, nodeID)
			continue
		case pmdoc.ItemKindOf(target) != "":
			items = append(items, target)
			continue
		}
		// A ref_key holds no '#', so the node id's document part ends at its first one.
		if document, _, _ := strings.Cut(nodeID, "#"); document != writing.Document {
			elsewhere = append(elsewhere, nodeID)
		} else if typ, ok := own[target.Block]; ok {
			found[nodeID] = pmdoc.BlockTarget{Types: []string{typ}, Document: writing.Document, Issue: writing.Issue}
		}
	}
	if len(elsewhere) > 0 {
		rows, err := q.Query(ctx, `
			select p.address, d.type, a.ref_key, coalesce(a.issue_key, '')
			from (select distinct unnest($1::text[]) as address) p
			cross join lateral (select `+ResolvedBlockSQL("p.address")+` as node_id) resolved
			join document_blocks d on d.node_id = resolved.node_id
			join artifacts a on a.id = `+BlockDocumentSQL("d")+`
		`, elsewhere)
		if err != nil {
			return pmdoc.BlockWriting{}, nil, fmt.Errorf("read the blocks %s points at: %w", artifactID, err)
		}
		defer rows.Close()
		for rows.Next() {
			var nodeID, typ, document, issue string
			if err := rows.Scan(&nodeID, &typ, &document, &issue); err != nil {
				return pmdoc.BlockWriting{}, nil, fmt.Errorf("scan a block %s points at: %w", artifactID, err)
			}
			target := found[nodeID]
			target.Types = append(target.Types, typ)
			target.Document, target.Issue = document, issue
			found[nodeID] = target
		}
		if err := rows.Err(); err != nil {
			return pmdoc.BlockWriting{}, nil, err
		}
	}
	// An item's R2 test reads the write's time, the database's clock once the write holds its
	// document's owner lock, read only for a write that points at an item.
	var at time.Time
	if len(items) > 0 {
		if err := q.QueryRow(ctx, `select clock_timestamp()`).Scan(&at); err != nil {
			return pmdoc.BlockWriting{}, nil, fmt.Errorf("read the time of the write to %s: %w", artifactID, err)
		}
	}
	held, err := conversationTargets(ctx, q, items, at)
	if err != nil {
		return pmdoc.BlockWriting{}, nil, err
	}
	aside := len(elsewhere) > 0
	for nodeID, target := range held {
		found[nodeID] = target
		aside = aside || target.Issue != writing.Issue
	}
	if len(issues) > 0 {
		var keys []string
		if err := q.QueryRow(ctx, `select coalesce(array_agg(key), '{}') from issues where key = any($1)`, issues).Scan(&keys); err != nil {
			return pmdoc.BlockWriting{}, nil, fmt.Errorf("read the issues %s points at: %w", artifactID, err)
		}
		for _, key := range keys {
			found[key] = pmdoc.BlockTarget{Issue: key}
		}
		if writing.Descendants, err = IssueDescendants(ctx, q, model.Actor{}, writing.Issue); err != nil {
			return pmdoc.BlockWriting{}, nil, err
		}
	}
	// Only a target in another document or in another issue's item can sit in another issue, so
	// the ancestor walk is paid for only there. A pending parent stands at the head of the
	// answer, the walk starting there (DocumentAddress.PendingParent).
	if aside {
		above := writing.Issue
		if address.PendingParent != "" {
			above = address.PendingParent
			writing.Ancestors = append(writing.Ancestors, address.PendingParent)
		}
		ancestors, err := IssueAncestors(ctx, q, above)
		if err != nil {
			return pmdoc.BlockWriting{}, nil, err
		}
		writing.Ancestors = append(writing.Ancestors, ancestors...)
	}
	return writing, found, nil
}

// blockFirstVersion is a left join lateral, aliased alias, over the first version that held block
// block of document artifact (SQL expressions of the outer row): F, the lowest version
// document_block_texts records a text of the block in, as alias.version, and F's authors and
// time as alias.authors and alias.created_at. alias.known is whether Dispatch knows the block was
// made in F: F is 1, or a block-aware build indexed version F-1, which did not hold it. A block
// first seen when its document was first indexed, or after a version no block-aware build
// indexed, may be older than F. A block with no recorded text leaves every column null.
//
// F is a version of the block's own document (BlockDocumentSQL): for a block of an issue room,
// whose rows are keyed by the room's tree artifact and record that document's own version
// numbers, the document its fragment is, whose versions and indexed versions F is read against
// (T12a of the issue-tree plan); for any other block, artifact itself. A block of an
// issue room that has stood in more than one fragment (MovedRoomBlockSQL) has rows recording
// versions of more than one document, and no row says which, so the lowest of them need not be a
// version of the document that first held it, nor F a version of the document it stands in now:
// Dispatch knows no first version of it, and every column is null, as for a block with no
// recorded text.
func blockFirstVersion(alias, artifact, block string) string {
	return blockFirstVersionOr(alias, artifact, block, "")
}

// blockFirstVersionOr is blockFirstVersion with document, SQL text answering a uuid, as the
// document a text row of the block records a version of where no issue_room_blocks row names the
// block (BlockDocumentOrSQL), in place of the row's own artifact; "" keeps the row's own. A reader
// of a block a version deleted from an issue room, whose issue_room_blocks row went with it, reads
// the room's rows of its text and passes the document it read the block's address in, which is the
// only record left of which fragment held it.
func blockFirstVersionOr(alias, artifact, block, document string) string {
	return blockFirstVersionMoved(alias, artifact, block, document, MovedRoomBlockSQL(artifact, block))
}

// blockFirstVersionMoved is blockFirstVersionOr with moved, SQL text true of a block of an issue
// room that has stood in more than one fragment, in place of MovedRoomBlockSQL: a reader of many
// blocks at once reads it by a probe a block of its own, a column of a lateral before this one
// (blockAuthors' movedInRoom), so the test costs one probe a block whatever the planner estimates.
func blockFirstVersionMoved(alias, artifact, block, document, moved string) string {
	owner := BlockDocumentSQL("t")
	if document != "" {
		owner = BlockDocumentOrSQL("t", document)
	}
	return `left join lateral (
		select t.version,
		       t.version = 1 or exists (
		         select 1 from document_indexed_versions i where i.artifact_id = owner.id and i.version = t.version - 1
		       ) as known,
		       v.authors, v.created_at
		from document_block_texts t
		cross join lateral (select ` + owner + ` as id) owner
		left join artifact_versions v on v.artifact_id = owner.id and v.number = t.version
		where t.artifact_id = ` + artifact + ` and t.block_id = ` + block + ` and t.text is not null
		  and not ` + moved + `
		order by t.version limit 1
	) ` + alias + ` on true`
}

// blockFirstColumns selects blockFirstVersion's columns, aliased alias, in blockFirst.targets'
// order.
func blockFirstColumns(alias string) string {
	return alias + `.version, ` + alias + `.known, ` + alias + `.authors, ` + alias + `.created_at`
}

// blockFirst is the first version that held a block, as blockFirstColumns selects it.
type blockFirst struct {
	version   *int
	known     *bool
	authors   []byte
	createdAt *time.Time
}

func (f *blockFirst) targets() []any {
	return []any{&f.version, &f.known, &f.authors, &f.createdAt}
}

// maker is who made the block and when: the authors and time of the version that first held it
// when Dispatch knows the block was made there, else nobody recorded at no time. Authors that do
// not decode (storedActors) fail.
func (f blockFirst) maker() ([]model.Actor, *time.Time, error) {
	if f.known == nil || !*f.known {
		return []model.Actor{}, nil, nil
	}
	authors, err := storedActors(f.authors)
	if err != nil {
		return nil, nil, err
	}
	return authors, f.createdAt, nil
}

// zeroActorSQL recognizes the recorded encoding of model.Actor{}, and the empty object
// in older rows. A SQL null is a missing row, not a stored zero actor.
func zeroActorSQL(column string) string {
	return `coalesce(` + column + ` in ('{"kind":"","id":""}'::jsonb, '{}'::jsonb), false)`
}

// roomBlockMakerJoin probes, for a block of an artifact of kind tree, the block's graph_nodes row
// by primary key for whether made_by is the zero actor and its made_at. For a non-tree artifact
// the probe stops at artifacts' primary key and answers nulls.
func roomBlockMakerJoin(room, address string) string {
	return `left join lateral (
		select ` + zeroActorSQL("n.made_by") + ` as zero_maker, n.made_at
		from (select id from artifacts where id = ` + room + ` and kind = 'tree' offset 0) tree
		cross join lateral (
			select n.made_by, n.made_at from graph_nodes n
			where n.kind = '` + BlockNodeKind + `' and n.id = ` + address + ` and tree.id = ` + room + `
			limit 1
		) n
		limit 1
	) maker on true`
}

// blockMaker resolves a block node's MadeBy and MadeAt by one rule across the walk
// (readBlockNodeRows), the removed-block read (RemovedBlockNode) and the head projection
// (HeadRow.BlockNode):
//
// For a block whose document_blocks row is a tree artifact's and whose node the store holds
// (treeBlock && nodeMadeAt != nil):
//  1. authors is the node's current authored_by actors in seq order (recorded), or [] when none.
//  2. Where the node's made_by is the zero actor (zeroMaker): MadeBy is authors, MadeAt is nil.
//  3. Else, where it states an authored_by (recorded != nil): MadeBy is authors, MadeAt is nodeMadeAt.
//  4. Else (states no authored_by and holds a maker, such as a decision): first.maker().
//
// For every other block (a document that is not a tree artifact, or a block with no stored node):
// first.maker(), with MadeBy replaced by storedActors(recorded) when recorded != nil.
func blockMaker(treeBlock bool, nodeMadeAt *time.Time, zeroMaker bool, recorded []byte, first blockFirst) ([]model.Actor, *time.Time, error) {
	if treeBlock && nodeMadeAt != nil {
		if zeroMaker {
			authors, err := storedActors(recorded)
			return authors, nil, err
		}
		if recorded != nil {
			authors, err := storedActors(recorded)
			return authors, nodeMadeAt, err
		}
		return first.maker()
	}
	madeBy, madeAt, err := first.maker()
	if err != nil {
		return nil, nil, err
	}
	if recorded != nil {
		madeBy, err = storedActors(recorded)
	}
	return madeBy, madeAt, err
}

// RemovedBlockNode is the node nodeID of block blockID of document (its resolved artifact node),
// which no current version holds since version removedVersion deleted it, built as loadBlockNodes
// builds a current block's but with no text or changed version: made as a current block is
// (blockFirstVersion), last changed by that deletion, with its version's authors and time, part of
// the document at the latest version a block-aware build indexed below removedVersion, the last
// one known to hold it (null when none is), and at the state and visibility the store holds for
// it (storedNode), which the index pass that deleted it stated gone. The caller adds its pin.
// Authors that do not decode (storedActors) fail the read. The block's text rows are the room's
// the document is edited in (DocumentRoomSQL), at the document's own version numbers, and the
// deletion took the issue_room_blocks row that named the document, so they are read as the
// document's (blockFirstVersionOr); its versions and indexed versions are the document's own.
func RemovedBlockNode(ctx context.Context, q Queryer, document model.GraphNode, nodeID, blockID string, removedVersion int) (model.GraphNode, error) {
	var first blockFirst
	var removedBy []byte
	var removedAt *time.Time
	var held *int
	var stored StoredNode
	var recorded []byte
	var roomZeroMaker *bool
	var roomMadeAt *time.Time
	documentID := document.ID
	storedJoin, storedColumns := storedNode(BlockNodeKind, "$4")
	if err := q.QueryRow(ctx, `
		select `+blockFirstColumns("first")+`, removed.authors, removed.created_at,
		       `+indexedBelowSQL("b.artifact_id", "$3")+`,
		       `+storedColumns+`, maker.zero_maker, maker.made_at,
		       (select jsonb_agg(held.who order by held.seq)
		        from (`+graphstore.CurrentTupleRead(`t.to_kind, t.to_id`, `t.who, t.seq, t.asserted`,
		`t.from_kind = 'block' and t.from_id = $4 and t.kind = 'authored_by' and t.to_kind <> 'value'`)+`
		        ) held where held.asserted and maker.made_at is not null)
		from (select $1::uuid as artifact_id, `+DocumentRoomSQL("$1::uuid")+` as room, $2::text as block_id) b
		`+blockFirstVersionOr("first", "b.room", "b.block_id", "b.artifact_id")+`
		left join artifact_versions removed on removed.artifact_id = b.artifact_id and removed.number = $3
		`+storedJoin+`
		`+roomBlockMakerJoin("b.room", "$4")+`
	`, documentID, blockID, removedVersion, nodeID).Scan(append(first.targets(), &removedBy, &removedAt, &held, &stored.State, &stored.Visibility, &roomZeroMaker, &roomMadeAt, &recorded)...); err != nil {
		return model.GraphNode{}, fmt.Errorf("read the facts of removed block %q of %s: %w", blockID, documentID, err)
	}
	zeroMaker := roomZeroMaker != nil && *roomZeroMaker
	madeBy, madeAt, err := blockMaker(roomMadeAt != nil, roomMadeAt, zeroMaker, recorded, first)
	if err != nil {
		return model.GraphNode{}, fmt.Errorf("decode the maker of removed block %q of %s: %w", blockID, documentID, err)
	}
	changedBy, err := storedActors(removedBy)
	if err != nil {
		return model.GraphNode{}, fmt.Errorf("decode the last change of removed block %q of %s: %w", blockID, documentID, err)
	}
	node := model.GraphNode{
		Kind:          "block",
		ID:            nodeID,
		BlockID:       blockID,
		IssueKey:      document.IssueKey,
		Project:       document.Project,
		Ref:           BlockRef(document.Ref, blockID),
		Document:      document.Ref,
		MadeBy:        madeBy,
		MadeAt:        madeAt,
		ChangedBy:     changedBy,
		ChangedAt:     removedAt,
		Parent:        &model.NodeParent{Kind: "artifact", ID: documentID, Ref: document.Ref},
		ParentVersion: held,
	}
	setStored(&node, stored)
	return node, nil
}

// indexedBelowSQL is the latest version of document artifact a block-aware build indexed below
// version, as a scalar expression over the SQL artifact and version, null where none is: the last
// version known to hold a block that version deleted, which is the version a removed block's own
// node belongs to (RemovedBlockNode) and the version a lens on it names it at (pinnedParentSQL).
func indexedBelowSQL(artifact, version string) string {
	return `(select max(indexed.version) from document_indexed_versions indexed
		where indexed.artifact_id = ` + artifact + ` and indexed.version < ` + version + `)`
}

// uniqueBlocks keeps the first block of each id, in order, as the node GET …/blocks addresses by
// that id: a rendered version can repeat an id (pmdoc/block_ids.go), and document_blocks holds one
// row per id. It skips an empty id, and one that is not valid UTF-8 or holds a NUL, which a
// Postgres text[] cannot hold. It skips an id longer than text.MaxBlockIDBytes too, which a
// B-tree key of the index cannot hold, and reports how many blocks it skipped for that and the
// longest such id's bytes.
func uniqueBlocks(blocks []model.ArtifactBlock) (nodes []model.ArtifactBlock, long, longest int) {
	nodes = make([]model.ArtifactBlock, 0, len(blocks))
	seen := make(map[string]struct{}, len(blocks))
	for _, block := range blocks {
		if block.ID == "" || !text.Storable(block.ID) {
			continue
		}
		if len(block.ID) > text.MaxBlockIDBytes {
			long, longest = long+1, max(longest, len(block.ID))
			continue
		}
		if _, dup := seen[block.ID]; dup {
			continue
		}
		seen[block.ID] = struct{}{}
		nodes = append(nodes, block)
	}
	return nodes, long, longest
}

// pastBoundBlocks keeps, in order, the first block of each id longer than text.MaxBlockIDBytes,
// the blocks uniqueBlocks skips for their length, of the ids a Postgres text[] can hold: no node
// of the store, whose documents an issue room records all the same (planRoomBlocks).
func pastBoundBlocks(blocks []model.ArtifactBlock) []model.ArtifactBlock {
	var long []model.ArtifactBlock
	seen := map[string]struct{}{}
	for _, block := range blocks {
		if len(block.ID) <= text.MaxBlockIDBytes || !text.Storable(block.ID) {
			continue
		}
		if _, dup := seen[block.ID]; dup {
			continue
		}
		seen[block.ID] = struct{}{}
		long = append(long, block)
	}
	return long
}

// innermostBlocks names, for each start (ascending), the index of the tightest block whose
// [From, To) holds it, or -1. blocks are in document order, each parent before its children.
func innermostBlocks(blocks []model.ArtifactBlock, starts []int) []int {
	holders := make([]int, len(starts))
	stack := make([]int, 0, 8)
	next := 0
	for i, start := range starts {
		for next < len(blocks) && blocks[next].From <= start {
			for len(stack) > 0 && blocks[stack[len(stack)-1]].To <= blocks[next].From {
				stack = stack[:len(stack)-1]
			}
			stack = append(stack, next)
			next++
		}
		for len(stack) > 0 && blocks[stack[len(stack)-1]].To <= start {
			stack = stack[:len(stack)-1]
		}
		holders[i] = -1
		if len(stack) > 0 {
			holders[i] = stack[len(stack)-1]
		}
	}
	return holders
}
