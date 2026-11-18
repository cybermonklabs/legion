package refs

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/rank"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// blockNodeAddress is a block's address in the store, which document_blocks.node_id and
// block_refs.from_node hold and which the graph addresses a block by: the document's ref_key,
// '#' and the block's id.
func blockNodeAddress(refKey, blockID string) string { return refKey + "#" + blockID }

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
// row does not hold). supertag is BlockSupertag for every block but a decision and a reading,
// which take their type's (TypedFacts.supertag), so the typed-node pass and this index write one
// node and neither moves what the other wrote.
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
func blockNode(address string, document indexedDocument, maker model.Actor, at time.Time, state, supertag string, changed bool) graphstore.Node {
	return graphstore.Node{
		Kind: BlockNodeKind, ID: address, Supertag: supertag,
		Visibility: graphstore.VisibilityProject, State: state,
		Project: document.project, Parties: document.parties,
		MadeAt: at, MadeBy: maker, SourceEvent: graphstore.NoEvent,
		Restates: true, Changed: changed,
	}
}

// indexedDocument is what the version's own row and its document's row say of the pass: the
// document's ref_key, which every block's address starts with; the authors the version's row
// records, in its own order, and the first of them, its writer, which every tuple this pass writes
// carries; the project the document belongs to, or for an agent conversation's file the session
// it is between, which every block node the pass writes carries (graphstore.Node's Project and
// Parties); and the issue that holds the document, "" for one a project holds, which a decision's
// or an ask's tie to another issue is read against (DecisionFacts), and an authored node's place
// (placeAuthored). A conversation holds no markdown document (api's agentOwnsFilesOnly), so no
// block has parties today; the read keeps the one rule every document's node follows.
//
// band is the first byte of every rank a block of the document stands at (DocumentBandSQL), and
// at is the pass's own time: every placement and every value the pass states is dated there
// (dispatch://INFRA-1253/spec#b-r-one-tree). It is the database's clock_timestamp(), read once,
// after the pass holds the document's owner row and the revision fence (IndexBlocks reads this
// record after standsAtVersion), and not the transaction's now(): a pass that waited at the owner
// row behind another pass of the same document began before that one committed and appends its
// tuples after it, so only a time read once the wait is over orders the tuples' times as their
// seqs are ordered. It is later than the version the pass describes, on the request path as on
// the block-index reconciliation's, so no tuple a renderer reads claims to describe a moment
// before the pass that wrote it.
type indexedDocument struct {
	refKey  string
	authors []model.Actor
	writer  model.Actor
	project string
	parties []string
	issue   string
	band    byte
	at      time.Time
}

// top is the node a top-level block of the document hangs under: the issue's node for a block of
// an issue's spec, Verification or plan document (and, once the issue room exists, of its tree's
// own fragment), so one walk from the issue reaches every node of it, and the document's own node
// for an upload or a project document, whose body is that document's. document_blocks keeps a
// null parent_block_id for either, and in_document still says which document the block is in.
func (d indexedDocument) top() (kind, id string) {
	if d.band != RankBandOther && d.issue != "" {
		return graphstore.KindIssue, d.issue
	}
	return graphstore.KindArtifact, d.refKey
}

// The bands a block's rank stands in (D15 of dispatch://INFRA-1253's plan): its first byte says
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
// upload, a project's document - stands in the last band.
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

// DocumentTopSQL is the far end a top-level block of the document of the artifacts row aliased
// alias hangs under, written kind:id as the census writes a link (indexedDocument.top): the
// issue's node for a document of a band other than RankBandOther, which only an issue's document
// takes, and the document's own node for every other.
func DocumentTopSQL(alias string) string {
	return `(case when ` + DocumentBandSQL(alias) + ` <> '` + string(RankBandOther) + `' then '` + graphstore.KindIssue + `:' || ` +
		alias + `.issue_key else '` + graphstore.KindArtifact + `:' || ` + alias + `.ref_key end)`
}

// BlockDocumentSQL is the document the document_blocks row aliased alias is a block of, as SQL
// text answering a uuid: the document its issue_room_blocks row names, for a block of an issue room
// (a row keyed by the room's tree artifact, whose fragment's document that row records: T12a of
// dispatch://INFRA-1253's plan), else the row's own artifact_id. Every reader that takes a row's
// artifact as the block's document - to find its role, issue, slug or live blocks, or to map the
// version it records to that document's versions, their times and events, its indexed versions
// or its keeps - takes it from here, and keys a version by this document and the number: a
// room's rows carry each document's own version numbers under one key. A block's node address,
// and every pointer at it, keep the row's own artifact.
func BlockDocumentSQL(alias string) string {
	return `coalesce((select room_block.document_id from issue_room_blocks room_block
		where room_block.tree_artifact_id = ` + alias + `.artifact_id and room_block.block_id = ` + alias + `.block_id), ` +
		alias + `.artifact_id)`
}

// MovedRoomBlockSQL is true of the block block (SQL text) of the artifact artifact (SQL text
// answering a uuid) while that artifact is an issue's tree and the block's node has stood in more
// than one document of its room: its in_document tuples, asserted at any time, name more than one
// (a block cut from one fragment and pasted into another, T12a of dispatch://INFRA-1253's plan).
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

// indexedVersionRecord reads indexedDocument in one statement, under the owner row the pass's
// caller already holds: the document of artifactID, and the authors of version `version` of the
// document writtenIn, which is artifactID itself but for a pass over an issue room, whose writer
// is that of the newest version it covers (Versions.Newest). It answers the version's whole
// author list, in the order its row stores it, which a block that version first holds records one
// authored_by for each of (blockAuthors), and the first of them as the writer. A version no row
// records - a caller indexing blocks of its own, as the seed does - leaves no author and the
// writer nobody, the zero actor, exactly as a node of the store written on a credential that
// names nobody carries.
func indexedVersionRecord(ctx context.Context, tx pgx.Tx, artifactID, writtenIn string, version int) (indexedDocument, error) {
	var document indexedDocument
	var project, session, issue *string
	var authors []byte
	var band string
	if err := tx.QueryRow(ctx, `
		select a.ref_key, a.project_key, a.session_id, a.issue_key, v.authors, `+DocumentBandSQL("a")+`, clock_timestamp()
		from artifacts a
		left join artifact_versions v on v.artifact_id = $3::uuid and v.number = $2
		where a.id = $1::uuid
	`, artifactID, version, writtenIn).Scan(&document.refKey, &project, &session, &issue, &authors, &band, &document.at); err != nil {
		return indexedDocument{}, fmt.Errorf("read version %d of %s for its block index: %w", version, writtenIn, err)
	}
	document.band = band[0]
	if project != nil {
		document.project = *project
	}
	if session != nil {
		document.parties = []string{graphstore.PartySession(*session)}
	}
	if issue != nil {
		document.issue = *issue
	}
	var err error
	if document.authors, err = storedActors(authors); err != nil {
		return indexedDocument{}, fmt.Errorf("decode the authors of version %d of %s: %w", version, artifactID, err)
	}
	if len(document.authors) > 0 {
		document.writer = document.authors[0]
	}
	return document, nil
}

// documentVersion is one version of one document: the document's artifact id and the version's
// number.
type documentVersion struct {
	document string
	number   int
}

// versionAuthors answers held with the authors of each version of wanted it does not hold yet,
// read in one statement: each version's authors in the order its row stores them (docs'
// actorSlice, for a settlement's), and none for a version no row records.
func versionAuthors(ctx context.Context, tx pgx.Tx, held map[documentVersion][]model.Actor, wanted []documentVersion) (map[documentVersion][]model.Actor, error) {
	var documents []string
	var numbers []int32
	for _, version := range wanted {
		if _, known := held[version]; known {
			continue
		}
		held[version] = nil
		documents, numbers = append(documents, version.document), append(numbers, int32(version.number))
	}
	if len(documents) == 0 {
		return held, nil
	}
	rows, err := tx.Query(ctx, `
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
// records its authors by authored_by tuples and by no attribute (T7 of dispatch://INFRA-1253's
// plan): an authored node anywhere, and every block of an issue's tree artifact (RankBandTree),
// but a decision and a reading, whose authors the typed-node pass states, a decision's by its
// decided_by (IndexDecisionNodes). RecordsAuthorsSQL is the same rule over a document_blocks row
// and the artifacts row it is keyed by, each named by its alias, which the store census reads the
// authors a store must hold the nodes of by (graphcensus).
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
// (indexedDocument.at). An author that names nobody the store can key states nothing, and two
// actors naming one node state it once.
//
// The version that first holds a block is the one blockFirstVersion names, the lowest
// document_block_texts records a text of it in, of its own document, where the block has such a
// row: a block written before this build, or by a pass that wrote no author - the drain of
// dispatch://INFRA-1253's T6 marks every stored document for a pass, so every such node takes its
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
func blockAuthors(ctx context.Context, tx pgx.Tx, artifactID string, document indexedDocument, candidates []string, firstWritten func(id string) []model.Actor) ([]graphstore.Node, []graphstore.UncausedTuple, error) {
	if len(candidates) == 0 {
		return nil, nil, nil
	}
	rows, err := tx.Query(ctx, `
		select exists (
			select 1 from (
				`+graphstore.CurrentTupleRead(`t.to_kind, t.to_id`, `t.asserted`,
		`t.from_kind = $2 and t.from_id = $3 || '#' || block.id and t.kind = $4
				  and t.to_kind <> '`+graphstore.KindValue+`'`)+`
			) held where held.asserted),
		       first.version is not null, first.authors
		from unnest($1::text[]) with ordinality as block (id, position)
		`+blockFirstVersion("first", "$5::uuid", "block.id")+`
		order by block.position
	`, candidates, BlockNodeKind, document.refKey, graphstore.TupleAuthoredBy, artifactID)
	if err != nil {
		return nil, nil, fmt.Errorf("read who wrote the first versions of the blocks of %s: %w", document.refKey, err)
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
			return nil, nil, fmt.Errorf("read who wrote the first version of a block of %s: %w", document.refKey, err)
		}
		if recorded || read >= len(candidates) {
			continue
		}
		id := candidates[read]
		authors := firstWritten(id)
		if recordedText {
			if authors, err = storedActors(firstAuthors); err != nil {
				return nil, nil, fmt.Errorf("decode the authors of the first version of block %s of %s: %w", id, document.refKey, err)
			}
		}
		address := blockNodeAddress(document.refKey, id)
		named := map[blockFarEnd]struct{}{}
		for _, author := range authors {
			authorNodes, facts, ok := graphstore.AuthorFacts(BlockNodeKind, address, author, document.at, graphstore.NoEvent)
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
				if _, again := made[blockFarEnd{kind: authorNode.Kind, id: authorNode.ID}]; !again {
					made[blockFarEnd{kind: authorNode.Kind, id: authorNode.ID}] = struct{}{}
					nodes = append(nodes, authorNode)
				}
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("read who wrote the first versions of the blocks of %s: %w", document.refKey, err)
	}
	if read != len(candidates) {
		return nil, nil, fmt.Errorf("read who wrote the first versions of %d blocks of %s and got %d back", len(candidates), document.refKey, read)
	}
	return nodes, tuples, nil
}

// scanFreshBlocks reads back the rows the document_blocks statement wrote: the created_at of
// each row it truly inserted, by block id. A row it updated is left out, since the block was
// already a part of the document and states no second membership.
func scanFreshBlocks(rows pgx.Rows, err error) (map[string]time.Time, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	fresh := map[string]time.Time{}
	for rows.Next() {
		var id string
		var created time.Time
		var inserted bool
		if err := rows.Scan(&id, &created, &inserted); err != nil {
			return nil, err
		}
		if inserted {
			fresh[id] = created
		}
	}
	return fresh, rows.Err()
}

// blockFarEnd is one far end a block's part_of points at now.
type blockFarEnd struct{ kind, id string }

// blockPlacement is where a pass placed the blocks of a version: ids are the blocks it states
// a parent for, in document order, and beside each its index among the version's unique blocks
// (placed), the index there of the block holding it (parents, -1 for one at the document's top
// level and for one that hangs under a node that is no block of the version), and that node
// where a block hangs under one (ends, nil for every other block, and nil as a whole where there
// is none): a reading in an answered ask, under the ask's node, and an authored node under a
// conversation node, a block of another document, or an issue that is not the document's top
// (placeAuthored).
type blockPlacement struct {
	ids             []string
	placed, parents []int
	ends            []*blockFarEnd
}

// end sets the far end the block at index hangs under, making ends where there is none yet.
func (p *blockPlacement) end(index int, far blockFarEnd) {
	if p.ends == nil {
		p.ends = make([]*blockFarEnd, len(p.ids))
	}
	p.ends[index] = &far
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

// probe is the parentProbe of every block of the placement but those kept, which state nothing.
func (p blockPlacement) probe(parentIDs []*string, kept []bool) parentProbe {
	probe := parentProbe{ids: p.ids, parents: parentIDs}
	if slices.Contains(kept, true) {
		probe.ids, probe.parents = nil, nil
		for index, keep := range kept {
			if !keep {
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

// reconcileBlockParents states the parent of every block of the version and projects it into
// document_blocks. It returns where it placed each block, which the pass's values are stated
// against (reconcileBlockValues), and the tuples it states, which its caller appends to the
// pass's write as it appends the reference pass's.
//
// It reads the parent each block holds in the store, over every block of the document and not
// the changed ones alone, so a block another block now holds, whose own text no version
// changed, gets its new parent here. It states a tuple only where the render disagrees with
// what the store asserts: one retraction for the parent that changed, one assertion for the new
// one, both dated at the pass's own time (indexedDocument.at). A re-index of a document nothing
// moved states nothing, which is what keeps a second pass from asserting a parent twice, since
// no code may update or delete a tuple row.
//
// A block no block holds hangs under the document's top (indexedDocument.top): the issue for a
// block of an issue's spec, Verification or plan document, and the document's own node for any
// other. A block an earlier build placed under its document's node is moved under the issue by
// the first pass that reaches it, as any other block that moved is. An authored node is the one
// block the render does not place: its author does (placeAuthored, D4 of dispatch://INFRA-1253's
// plan), and one whose part_of stands is no part of the comparison at all.
//
// The comparison is the read's own (heldBlockParentDifferences): the parent the render gives
// travels with the probe, so a document nothing moved reads back no row at all rather than one
// row per block for Go to compare. Both statements name a block by its id and build its node
// address from the document's ref_key, as the insert above does, so a pass over thousands of
// blocks holds no address for the thousands it states nothing about.
//
// The projection is a statement of its own rather than a column of the insert above, so that
// insert keeps firing only for a block whose text changed: a block that only moved keeps its
// changed_version and changed_at. The statement carries the node columns beside the parent, so
// a row the insert did not touch learns the address of its node here. A block at the top
// projects a null parent_block_id, whichever node its part_of names, and so does an authored node
// under any node that is no block of the indexed artifact.
//
// askParents is each reading of the version that stands in an answered ask, by its id, with that
// ask's id (DecisionFacts): its part_of is the ask's node, since the decision a reading there
// reads is the person's answer the node holds, while document_blocks.parent_block_id keeps the
// id of the ask block holding it, which is that tuple's projection. only, where it is not nil,
// is the blocks the pass states a parent for, the typed-node pass's own above the deferral bound
// (IndexDecisionNodes); every block is placed in the version all the same, so the parent each of
// them takes is the one a pass over the whole version gives it. authored is what the version
// writes of where its authored nodes go (authoredSourcesOf), the zero value for a pass that reads
// none.
func reconcileBlockParents(
	ctx context.Context,
	tx pgx.Tx,
	artifactID string,
	document indexedDocument,
	nodes []model.ArtifactBlock,
	fits []bool,
	askParents map[string]string,
	only map[string]struct{},
	authored authoredSources,
) (blockPlacement, []graphstore.UncausedTuple, error) {
	if len(nodes) == 0 {
		return blockPlacement{}, nil, nil
	}
	refKey := document.refKey
	placed, parents := placeBlocks(nodes, fits)
	if only != nil {
		kept := 0
		for index, block := range placed {
			if _, own := only[nodes[block].ID]; own {
				placed[kept], parents[kept] = block, parents[index]
				kept++
			}
		}
		placed, parents = placed[:kept], parents[:kept]
	}
	if len(placed) == 0 {
		return blockPlacement{}, nil, nil
	}
	placement := blockPlacement{ids: make([]string, len(placed)), placed: placed, parents: parents}
	blockIDs := placement.ids
	parentIDs := make([]*string, len(placed))
	for index, block := range placed {
		blockIDs[index] = nodes[block].ID
		if parents[index] >= 0 {
			parentIDs[index] = &nodes[parents[index]].ID
		}
		if ask, held := askParents[blockIDs[index]]; held {
			placement.end(index, blockFarEnd{kind: graphstore.KindAsk, id: ask})
		}
	}
	kept, err := placeAuthored(ctx, tx, document, nodes, &placement, parentIDs, authored)
	if err != nil {
		return blockPlacement{}, nil, fmt.Errorf("place the authored nodes of %s: %w", artifactID, err)
	}
	topKind, topID := document.top()
	probe := placement.probe(parentIDs, kept)
	differences, err := heldBlockParentDifferences(ctx, tx, refKey, topKind, topID, probe)
	if err != nil {
		return blockPlacement{}, nil, fmt.Errorf("read the parents the blocks of %s hold: %w", artifactID, err)
	}
	var tuples []graphstore.UncausedTuple
	for _, difference := range differences {
		index := probe.position(difference.block)
		address := blockNodeAddress(refKey, blockIDs[index])
		for _, far := range difference.retract {
			tuples = append(tuples, parentTuple(address, far.kind, far.id, document.at, document.writer, false))
		}
		if difference.holds {
			continue
		}
		toKind, toID := topKind, topID
		switch parent := parentIDs[index]; {
		case placement.ends != nil && placement.ends[index] != nil:
			toKind, toID = placement.ends[index].kind, placement.ends[index].id
		case parent != nil:
			toKind, toID = BlockNodeKind, blockNodeAddress(refKey, *parent)
		}
		tuples = append(tuples, parentTuple(address, toKind, toID, document.at, document.writer, true))
	}
	if _, err := tx.Exec(ctx, `
		update document_blocks d
		set node_kind = $3, node_address = $4 || '#' || p.block_id, parent_block_id = p.parent
		from unnest($2::text[], $5::text[]) as p (block_id, parent)
		where d.artifact_id = $1::uuid and d.block_id = p.block_id
		  and (d.parent_block_id is distinct from p.parent
		       or d.node_kind is distinct from $3
		       or d.node_address is distinct from $4 || '#' || p.block_id)
	`, artifactID, blockIDs, BlockNodeKind, refKey, parentIDs); err != nil {
		return blockPlacement{}, nil, fmt.Errorf("project the parents of the blocks of %s: %w", artifactID, err)
	}
	return placement, tuples, nil
}

// The block types whose node is an authored node (D4 of dispatch://INFRA-1253's plan): its parent
// is its author's to decide, which its text cannot, since a check sits after the spec's words and
// not inside a requirement. AuthoredTypesSQL is the same list as SQL, which the store census reads
// an authored node's row by (graphcensus).
const (
	criterionType   = "criterion"
	requirementType = "requirement"
	checkType       = "check"
	planTaskType    = "plan_task"

	AuthoredTypesSQL = `('` + criterionType + `', '` + requirementType + `', '` + checkType + `', '` + planTaskType + `')`
)

// servesKind is the pointer an authored node names what it serves by: a relation of its own,
// many to many, whose first target of the node's own issue is the node's first parent and no
// more (dispatch://INFRA-1253/spec#b-r-serves-coverage).
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
// the requirement came from (R3 and R22 of dispatch://INFRA-1253/spec): from the last "From:"
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
// held in the version the store last stated it in, which replaceBlockRefs keeps (the zero far
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
func heldAuthoredBlocks(ctx context.Context, tx pgx.Tx, refKey string, ids []string) ([]heldAuthored, error) {
	current := func(kind string) string {
		return `select held.to_kind, held.to_id from (
				` + graphstore.CurrentTupleRead(`t.to_kind, t.to_id`, `t.to_kind, t.to_id, t.asserted, t.seq`,
			`t.from_kind = $2 and t.from_id = $3 || '#' || block.id and t.kind = `+kind+`
				  and t.to_kind <> '`+graphstore.KindValue+`'`) + `
			) held
			where held.asserted
			order by held.seq desc limit 1`
	}
	rows, err := tx.Query(ctx, `
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
// replaceBlockRefs states its tuple, the zero far end where it names none or names a target the
// store cannot key, of which no tuple is stated either.
func writtenParent(sources authoredSources, id string) blockFarEnd {
	ref, written := sources.parent[id]
	if !written {
		return blockFarEnd{}
	}
	target, ok := TargetOf(ref)
	if !ok || !graphstore.AddressFits(target.ID) {
		return blockFarEnd{}
	}
	return blockFarEnd{kind: target.Kind, id: target.ID}
}

// placeAuthored places the authored nodes of placement (D4 of dispatch://INFRA-1253's plan),
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
// document into since this one last stated it (standsAtVersion's stale) changes none of that:
// the release before writes no tuple, so the placement, the part_of and the parent value the
// store holds are this release's own, and the projection a kept node's part_of gives is written
// again below (reconcileBlockParents), over whatever row that release wrote.
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
// that issue's owner row (IndexBlocks' caller), so the second waits for the first to commit, and
// under READ COMMITTED each of its statements, the chain read among them, reads what it committed.
func placeAuthored(ctx context.Context, tx pgx.Tx, document indexedDocument, nodes []model.ArtifactBlock, placement *blockPlacement, parentIDs []*string, sources authoredSources) ([]bool, error) {
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
	held, err := heldAuthoredBlocks(ctx, tx, document.refKey, ids)
	if err != nil {
		return nil, err
	}
	places := authoredPlaces{document: document, nodes: nodes, placement: placement, parentIDs: parentIDs}
	kept := make([]bool, len(placement.ids))
	var placing []int
	// The blocks no version of this pass holds that kept nodes hang under, whose chains the walk
	// climbs on through as the store holds them.
	var keptEnds []string
	for position, index := range authored {
		stored := held[position]
		if stored.placement == PlacementAuthored && stored.parent.kind != "" &&
			stored.pointer == writtenParent(sources, ids[position]) {
			kept[index] = true
			places.set(index, stored.parent)
			if _, inVersion := places.ownBlock(stored.parent); stored.parent.kind == BlockNodeKind && inVersion < 0 {
				keptEnds = append(keptEnds, stored.parent.id)
			}
			continue
		}
		placing = append(placing, index)
	}
	if len(placing) == 0 {
		return kept, nil
	}
	resolved, err := resolveAuthored(ctx, tx, document, placement, placing, sources, keptEnds)
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

// authoredResolution is what the store says of the targets the authored nodes a pass places name
// (resolveAuthored): each ask, comment or message, or block of one, by node id, with its item's
// own issue and whether it is a conversation node at the pass's time (conversationTargets); each
// block of another document that is a node of the store, by node id, with its document's issue,
// "" for a document no issue holds; the chain of far ends the part_of of each such block, and of
// each block no version of the pass holds that a kept node hangs under, climbs through as the
// store holds it now, nearest first, at most authoredChainDepth long (PartOfChains, which the
// cycle walk climbs, authoredPlaces.cycles); and the issues above the indexed document's own,
// read only where a parent attribute names something of another issue.
type authoredResolution struct {
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
// many targets.
func resolveAuthored(ctx context.Context, tx pgx.Tx, document indexedDocument, placement *blockPlacement, placing []int, sources authoredSources, keptEnds []string) (authoredResolution, error) {
	var items []text.Ref
	var blocks []string
	own := document.refKey + "#"
	add := func(ref text.Ref, item, block bool) {
		switch {
		case pmdoc.ItemKindOf(ref) != "":
			if item {
				items = append(items, ref)
			}
		case block && ref.Kind == BlockNodeKind:
			if id := text.TargetID(ref); !strings.HasPrefix(id, own) && !slices.Contains(blocks, id) {
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
	var resolved authoredResolution
	if len(items) > 0 {
		var err error
		if resolved.items, err = conversationTargets(ctx, tx, items, document.at); err != nil {
			return authoredResolution{}, err
		}
	}
	starts := keptEnds
	if len(blocks) > 0 {
		rows, err := tx.Query(ctx, `
			select d.node_id, coalesce(a.issue_key, '') from document_blocks d
			join artifacts a on a.id = d.artifact_id
			where d.node_id = any($1) and d.node_kind is not null
		`, blocks)
		if err != nil {
			return authoredResolution{}, fmt.Errorf("read the blocks the authored nodes of %s name: %w", document.refKey, err)
		}
		resolved.blocks = make(map[string]string, len(blocks))
		for rows.Next() {
			var node, issue string
			if err := rows.Scan(&node, &issue); err != nil {
				rows.Close()
				return authoredResolution{}, fmt.Errorf("read a block the authored nodes of %s name: %w", document.refKey, err)
			}
			resolved.blocks[node] = issue
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return authoredResolution{}, fmt.Errorf("read the blocks the authored nodes of %s name: %w", document.refKey, err)
		}
		starts = slices.Concat(starts, slices.Collect(maps.Keys(resolved.blocks)))
	}
	if len(starts) > 0 {
		slices.Sort(starts)
		var err error
		if resolved.chains, err = PartOfChains(ctx, tx, slices.Compact(starts)); err != nil {
			return authoredResolution{}, fmt.Errorf("read where the blocks the authored nodes of %s stand under stand: %w", document.refKey, err)
		}
	}
	if document.issue == "" {
		return resolved, nil
	}
	for _, index := range placing {
		ref, written := sources.parent[placement.ids[index]]
		if !written {
			continue
		}
		target, _ := TargetOf(ref)
		issue, known := resolved.blocks[target.ID]
		if item, found := resolved.items[target.ID]; found {
			issue, known = item.Issue, true
		}
		if known && issue != document.issue {
			var err error
			if resolved.ancestors, err = IssueAncestors(ctx, tx, document.issue); err != nil {
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
// time a far end names a block of the indexed artifact or the cycle walk climbs a block.
type authoredPlaces struct {
	document  indexedDocument
	nodes     []model.ArtifactBlock
	placement *blockPlacement
	parentIDs []*string
	index     map[string]int
	placed    map[int]int
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
	id, own := strings.CutPrefix(far.id, p.document.refKey+"#")
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
	if issue := p.document.issue; issue != "" && p.nodes[node].Type == requirementType {
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
	if p.document.issue != "" {
		return append(candidates, blockFarEnd{kind: graphstore.KindIssue, id: p.document.issue})
	}
	kind, topID := p.document.top()
	return append(candidates, blockFarEnd{kind: kind, id: topID})
}

// target is the far end ref names where an authored node may hang under it: for its parent
// attribute (above), a conversation node, or a block of a document, of the node's own issue or of
// an issue above it; for a serves target, a block of a document of its own issue. A block of the
// indexed artifact is one only where the version holds it, since the pass places no other.
func (p *authoredPlaces) target(ref text.Ref, resolved authoredResolution, above bool) (blockFarEnd, bool) {
	target, ok := TargetOf(ref)
	if !ok || !graphstore.AddressFits(target.ID) {
		return blockFarEnd{}, false
	}
	far := blockFarEnd{kind: target.Kind, id: target.ID}
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
			return far, held >= 0
		}
		held, found := resolved.blocks[target.ID]
		if !found {
			return blockFarEnd{}, false
		}
		issue = held
	default:
		return blockFarEnd{}, false
	}
	if issue != "" && (issue == p.document.issue || above && slices.Contains(resolved.ancestors, issue)) {
		return far, true
	}
	return blockFarEnd{}, false
}

// The values the block index states of every block beside its parent (R1 and R15 of
// dispatch://INFRA-1253, D1 and D9 of its plan): AttributeRank, its place among its siblings, a
// base-62 string compared byte for byte (`collate "C"`), which stands in its document's band
// (RankBandSpec and the rest); graphstore.AttributeText, the block's whole slice of its version's
// markdown, its children's text included, which document_block_texts projects; and
// AttributePlacement, which says what decides the block's parent: PlacementText, the text it
// stands in, for every block the rendering places, and PlacementAuthored for an authored node,
// whose parent its author decides (placeAuthored).
const (
	AttributeRank      = "rank"
	AttributePlacement = "placement"
	PlacementText      = "text"
	PlacementAuthored  = "authored"
)

// heldBlockValue is what the store holds now of one block the values pass reads: its current
// rank, "" where it holds none, whether it holds a current text value, and its current placement,
// "" where it holds none.
type heldBlockValue struct {
	rank, placement string
	text            bool
}

// reconcileBlockValues states the rank, the text and the placement of every block placement
// placed, in the pass's own transaction and dated at its own time (indexedDocument.at). It
// returns the values it states, which the caller appends with the pass's other facts.
//
// It reads what the store holds of every one of those blocks in one statement (heldBlockValues)
// and states a value only where it moves: a rank where the block's place among its siblings moved
// (rankBlocks), a text where its text sum changed - changed is the blocks whose document_blocks
// row the pass wrote, so the text value and the document_block_texts row move together - or
// where the block holds no current text yet, and a placement where the block holds none or
// another: PlacementAuthored for an authored node (placeAuthored), PlacementText for every other
// block. So a block a version only moved takes its new parent and its new rank here although
// the pass writes no row of it, a re-index of a document nothing moved states nothing, and the
// first index of a document, or the first pass to reach a block an earlier build indexed, gives
// every block all three. A block the version dropped is no block the pass places: it keeps the
// values it held, and its node, stated gone, says it is no part of the version.
//
// stale is IndexBlocks' (standsAtVersion): another release indexed the document after the store
// last stated its blocks, and rewrote the rows and the document_block_texts of each block whose
// text its version moved while stating no text value. The sums the pass compared the render with
// are that release's, so a block whose text that release moved is in no changed set, and its text
// value still says what the store last stated. Such a pass states the text of every block it
// places, as the first index of a document does.
func reconcileBlockValues(
	ctx context.Context,
	tx pgx.Tx,
	document indexedDocument,
	markdown string,
	nodes []model.ArtifactBlock,
	placement blockPlacement,
	changed map[string]struct{},
	stale bool,
	fragments map[string]fragmentDocument,
) ([]graphstore.UncausedTuple, error) {
	if len(placement.ids) == 0 {
		return nil, nil
	}
	held, err := heldBlockValues(ctx, tx, document.refKey, placement.ids)
	if err != nil {
		return nil, fmt.Errorf("read the values the blocks of %s hold: %w", document.refKey, err)
	}
	ranks := rankBlocks(blockBands(document, nodes, fragments), placement, held)
	var tuples []graphstore.UncausedTuple
	value := func(id, attribute, to string) {
		tuples = append(tuples, graphstore.UncausedTuple{Tuple: graphstore.Tuple{
			FromKind: BlockNodeKind, FromID: blockNodeAddress(document.refKey, id), Kind: attribute,
			ToKind: graphstore.KindValue, ToID: to,
			At: document.at, Who: document.writer, Asserted: true,
		}})
	}
	for index, id := range placement.ids {
		if ranks[index] != held[index].rank {
			value(id, AttributeRank, ranks[index])
		}
		if _, wrote := changed[id]; wrote || stale || !held[index].text {
			block := nodes[placement.placed[index]]
			value(id, graphstore.AttributeText, markdown[block.From:block.To])
		}
		placed := PlacementText
		if authoredType(nodes[placement.placed[index]].Type) {
			placed = PlacementAuthored
		}
		if held[index].placement != placed {
			value(id, AttributePlacement, placed)
		}
	}
	return tuples, nil
}

// heldBlockValues reads, in one statement, the current rank, text and placement of each block of
// ids, as graphstore.CurrentValue reads one value - the newest tuple under the attribute's name,
// kept while it asserts - through graph_tuples_values, one probe a value. Each lateral writes
// to_kind = 'value' into its text rather than binding it, as CurrentValue does, because
// graph_tuples_values is partial on that predicate: a bound kind is a parameter the planner of a
// generic plan cannot match the index's predicate against. Of a block's text it reads whether one
// is held and not the text itself, which no comparison here needs. It answers the blocks in ids'
// order, and names each by its id, building its address from refKey as every other statement of
// the pass does.
func heldBlockValues(ctx context.Context, tx pgx.Tx, refKey string, ids []string) ([]heldBlockValue, error) {
	rows, err := tx.Query(ctx, `
		select coalesce(rank.held, ''), coalesce(text.held, false), coalesce(placement.held, '')
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
		order by block.position
	`, ids, BlockNodeKind, refKey, AttributeRank, graphstore.AttributeText, AttributePlacement)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	held := make([]heldBlockValue, 0, len(ids))
	for rows.Next() {
		var value heldBlockValue
		if err := rows.Scan(&value.rank, &value.text, &value.placement); err != nil {
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
func rankBlocks(bands []byte, placement blockPlacement, held []heldBlockValue) []string {
	nodes := len(bands)
	ranks := make([]string, len(placement.ids))
	// Each parent's children as a list in document order, threaded through next: a parent is
	// the index of the block holding the child plus one, 0 for the document's top, or past
	// nodes+1 for a node that is no block of the version, so no map of slices is built for a
	// document of thousands of blocks.
	parents := nodes + 1
	var ends map[blockFarEnd]int
	parentOf := make([]int, len(placement.ids))
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
	first, last := make([]int, parents), make([]int, parents)
	next := make([]int, len(placement.ids))
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
			if childBand := bands[placement.placed[child]]; childBand != band && len(ranker.children) > 0 {
				ranker.rank(band, held, ranks)
				ranker.children = ranker.children[:0]
			}
			band = bands[placement.placed[child]]
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
// whose blocks name none.
func fragmentDocuments(ctx context.Context, tx pgx.Tx, artifactID string, nodes []model.ArtifactBlock) (map[string]fragmentDocument, error) {
	var ids []string
	for _, block := range nodes {
		if block.Document != "" && block.Document != artifactID && !slices.Contains(ids, block.Document) {
			ids = append(ids, block.Document)
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := tx.Query(ctx, `
		select a.id::text, a.ref_key, `+DocumentBandSQL("a")+`
		from artifacts a where a.id = any($1::uuid[])
	`, ids)
	if err != nil {
		return nil, fmt.Errorf("read the documents whose fragments of %s's room hold its blocks: %w", artifactID, err)
	}
	defer rows.Close()
	documents := make(map[string]fragmentDocument, len(ids))
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
func (d indexedDocument) inDocument(block model.ArtifactBlock, fragments map[string]fragmentDocument) string {
	if held, ok := fragments[block.Document]; ok {
		return held.refKey
	}
	return d.refKey
}

// blockBands is the band each block of nodes stands in: its fragment's document's
// (fragmentDocuments), else the indexed document's own.
func blockBands(document indexedDocument, nodes []model.ArtifactBlock, fragments map[string]fragmentDocument) []byte {
	bands := make([]byte, len(nodes))
	for index, block := range nodes {
		bands[index] = document.band
		if held, ok := fragments[block.Document]; ok {
			bands[index] = held.band
		}
	}
	return bands
}

// reconcileRoomBlocks keeps issue_room_blocks, the projection of each in_document of an issue
// room's blocks (D16 of dispatch://INFRA-1253's plan), level with the version the pass indexes
// under the issue's tree artifact, artifactID, in the pass's transaction: the row of each block
// the version dropped goes with its document_blocks row, each block the version holds names its
// fragment's document (fragmentDocuments), or the tree artifact for a block of the tree's own
// fragment, and a block whose document moved - a block cut from one fragment and pasted into
// another - takes one retraction of the in_document it held and one assertion of the new one,
// dated at the pass's own time, which it answers for the caller to append with the pass's other
// facts. A block whose row it inserts states its in_document where its document_blocks row is
// inserted (IndexBlocks). A document that is not an issue's tree writes nothing here.
func reconcileRoomBlocks(
	ctx context.Context,
	tx pgx.Tx,
	artifactID string,
	document indexedDocument,
	nodes []model.ArtifactBlock,
	fits []bool,
	fragments map[string]fragmentDocument,
	deleted []string,
) ([]graphstore.UncausedTuple, error) {
	if document.band != RankBandTree {
		return nil, nil
	}
	if len(deleted) > 0 {
		if _, err := tx.Exec(ctx, `
			delete from issue_room_blocks where tree_artifact_id = $1::uuid and block_id = any($2)
		`, artifactID, deleted); err != nil {
			return nil, fmt.Errorf("delete the room blocks version of %s no longer holds: %w", artifactID, err)
		}
	}
	ids := make([]string, 0, len(nodes))
	documents := make([]string, 0, len(nodes))
	for index, block := range nodes {
		if !fits[index] {
			continue
		}
		owner := artifactID
		if _, ok := fragments[block.Document]; ok {
			owner = block.Document
		}
		ids, documents = append(ids, block.ID), append(documents, owner)
	}
	if len(ids) == 0 {
		return nil, nil
	}
	// The statement answers each row whose document it moved, with the document the row held, and
	// the ref_key of each of the two, so the retraction and the assertion need no second read.
	rows, err := tx.Query(ctx, `
		with wanted as (
			select w.block_id, w.document_id::uuid as document_id
			from unnest($2::text[], $3::text[]) as w (block_id, document_id)
		), held as (
			select r.block_id, r.document_id from issue_room_blocks r
			where r.tree_artifact_id = $1::uuid and r.block_id = any($2)
		), written as (
			insert into issue_room_blocks (tree_artifact_id, block_id, document_id)
			select $1::uuid, wanted.block_id, wanted.document_id from wanted
			on conflict (tree_artifact_id, block_id) do update set document_id = excluded.document_id
			where issue_room_blocks.document_id is distinct from excluded.document_id
		)
		select held.block_id, old.ref_key, new.ref_key
		from held
		join wanted on wanted.block_id = held.block_id and wanted.document_id <> held.document_id
		join artifacts old on old.id = held.document_id
		join artifacts new on new.id = wanted.document_id
	`, artifactID, ids, documents)
	if err != nil {
		return nil, fmt.Errorf("record the documents of the room blocks of %s: %w", artifactID, err)
	}
	defer rows.Close()
	var tuples []graphstore.UncausedTuple
	for rows.Next() {
		var id, was, now string
		if err := rows.Scan(&id, &was, &now); err != nil {
			return nil, fmt.Errorf("record the documents of the room blocks of %s: %w", artifactID, err)
		}
		address := blockNodeAddress(document.refKey, id)
		for _, membership := range []struct {
			to       string
			asserted bool
		}{{was, false}, {now, true}} {
			tuples = append(tuples, graphstore.UncausedTuple{Tuple: graphstore.Tuple{
				FromKind: BlockNodeKind, FromID: address, Kind: TupleInDocument,
				ToKind: graphstore.KindArtifact, ToID: membership.to,
				At: document.at, Who: document.writer, Asserted: membership.asserted,
			}})
		}
	}
	return tuples, rows.Err()
}

// childRanker ranks the children of one parent at a time, holding the slices that work takes
// from one parent to the next, so a document of many parents allocates them once.
type childRanker struct {
	children, tails, previous []int
	standing                  []string
	kept                      []bool
}

// rank writes into ranks the rank of each of the children: the longest run of the ranks they hold
// that stand in the band and already increase in document order, compared byte for byte, keeps
// its ranks (increasingRun), and every other child takes rank.Between the kept ranks on either
// side of it, or the band's own bounds where it has none, its first character band and the next.
// So a block moved under another parent, or among its siblings, takes a new rank and its siblings
// keep theirs; a block that holds a rank of another band - one moved into this document from
// another's fragment - takes one of this band; two siblings holding one rank cannot both be kept,
// so the later is given a new one; and no rank.Between is ever asked for bounds out of order. A
// run of children with no rank to keep, as at a document's first index, is spread evenly between
// its bounds (spreadRanks), so the ranks of a list of thousands of items stay a few bytes long.
func (r *childRanker) rank(band byte, held []heldBlockValue, ranks []string) {
	count := len(r.children)
	r.standing = slices.Grow(r.standing[:0], count)[:count]
	for index, child := range r.children {
		r.standing[index] = ""
		if standsInBand(held[child].rank, band) {
			r.standing[index] = held[child].rank
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

// PlaceBlocks is where the block index places each block of a version of the document refKey
// names. It answers the version's unique blocks (the first block of each id), the index among
// them of each block it states a part_of for, in document order, and beside each of those the
// index among them of the block that holds it, -1 for a block the document holds. blocks are
// the version's rendered blocks, as IndexBlocks takes them.
//
// It is the one walk the index states each part_of from (reconcileBlockParents) and the sizing
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

// blockParentDifference is one block whose part_of in the store is not what the render gives:
// where the block sits in the arrays the read was handed, the far ends the store still asserts
// and the render does not give, and whether the render's own parent is among the far ends it
// asserts. A block the store already places where the render does answers no difference at all.
type blockParentDifference struct {
	block   int
	retract []blockFarEnd
	holds   bool
}

// heldBlockParentDifferences is each block of a document whose parent in the store is not the
// one the render gives, read the way the store reads a current relation - the newest tuple per
// far end, kept while it asserts (graphstore.CurrentTupleRead) - but in one statement for every
// block of the document rather than one per block, which on a document of thousands of blocks
// would be thousands of round trips. The lateral reads through graph_tuples_current, whose column
// order is the one the distinct on walks.
//
// The probe names the blocks that are nodes of the store, in document order (ids), and beside
// each the block it sits in (parents), nil for a block at the document's top level, whose parent
// is the node topKind and topID name (indexedDocument.top), and the node it hangs under in place
// of either (endKinds and endIDs, nil or as long as ids: an answered ask's for a reading in it,
// and an authored node's own, placeAuthored), nil for every other block. The statement builds
// both block addresses from refKey, as the insert of document_blocks does, so the caller hands
// over one ref_key and not one address per block.
//
// The comparison is the statement's: a block answers a row only where the store asserts a far
// end the render does not give, or asserts none the render does, so an edit that moved nothing
// reads back no row and a document of thousands of blocks costs its probe and nothing more. The
// having clause is that rule, and the two array_aggs carry the far ends the pass retracts. A
// difference names its block by its place in the probe.
func heldBlockParentDifferences(ctx context.Context, tx pgx.Tx, refKey, topKind, topID string, probe parentProbe) ([]blockParentDifference, error) {
	if len(probe.ids) == 0 {
		return nil, nil
	}
	rows, err := tx.Query(ctx, `
		select block.ordinality,
		       array_agg(current.to_kind) filter (where not current.wanted),
		       array_agg(current.to_id) filter (where not current.wanted),
		       coalesce(bool_or(current.wanted), false)
		from unnest($1::text[], $2::text[], $8::text[], $9::text[]) with ordinality as block (id, parent, end_kind, end_id)
		left join lateral (
			select held.to_kind, held.to_id,
			       held.to_kind = case when block.end_kind is not null then block.end_kind
			                           when block.parent is null then $6 else $3 end
			       and held.to_id = case when block.end_kind is not null then block.end_id
			                             when block.parent is null then $10 else $7 || '#' || block.parent end
			         as wanted
			from (
				`+graphstore.CurrentTupleRead(`t.to_kind, t.to_id`, `t.to_kind, t.to_id, t.asserted`,
		`t.from_kind = $3 and t.from_id = $7 || '#' || block.id and t.kind = $4
				  and t.to_kind <> $5`)+`
			) held
			where held.asserted
		) current on true
		group by block.ordinality
		having not coalesce(bool_or(current.wanted), false)
		    or count(*) filter (where not current.wanted) > 0
		order by block.ordinality
	`, probe.ids, probe.parents, BlockNodeKind, graphstore.TuplePartOf, graphstore.KindValue, topKind, refKey, probe.endKinds, probe.endIDs, topID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var differences []blockParentDifference
	for rows.Next() {
		var ordinality int64
		var kinds, ids []string
		var holds bool
		if err := rows.Scan(&ordinality, &kinds, &ids, &holds); err != nil {
			return nil, err
		}
		difference := blockParentDifference{block: int(ordinality) - 1, holds: holds}
		for index, kind := range kinds {
			difference.retract = append(difference.retract, blockFarEnd{kind: kind, id: ids[index]})
		}
		differences = append(differences, difference)
	}
	return differences, rows.Err()
}

// heldLink is one link a block node states beside its membership of its document and its
// placement, of a kind a pair of a version writes into block_refs (heldLinkKinds): a mention's or
// a pointer's, under the kind its pair names, to its far end, or a decision's tie. A tuple of any
// other kind out of a block is no link of the version's: a requirement's constrained_by
// (graphstore.TupleConstrainedBy, the pointer route's), a changed_by_run a write made for a run
// appended (graphstore.TupleChangedByRun), a decision's kept_by (the Keep's), a reading's
// authored_by (IndexDecisionNodes) and an ask block copy's copied_from (docs' claimCopies). No
// block_refs row projects any of them, so no pair of a version writes them and no pass that drops
// a row retracts them.
type heldLink struct{ kind, toKind, toID string }

// heldLinkKinds is true of a tuple t of a kind a pair of a version writes into block_refs, the
// kinds whose links the release before this one drops by deleting their rows: a mention and a
// pointer of each `block[]` attribute the store's type nodes declare (BlockRefKindsSQL), and a
// decision's ties, its decided_by, from_ask and shaped (typedKinds, TypedFacts.pairs). It is the
// census's own reading of the tuples a block_refs row states (graphcensus's DigestedTuple).
var heldLinkKinds = `(t.kind in (` + BlockRefKindsSQL + `) or t.kind in (` + quotedKinds(typedKinds) + `))`

// heldDocumentBlocks is what the store states now of the blocks of the document refKey names, by
// block id: each block node that states itself a member of the document (an in_document tuple
// into the document's artifact address) and that the store holds present (at any state but gone,
// R12 of dispatch://INFRA-1950), with every link it states now of a kind a version's pairs write
// (heldLinkKinds) - a mention or a pointer (BlockRefKindsSQL), or a decision's or an ask block's
// tie (typedKinds) - read the way the store reads a current relation (graphstore.CurrentTupleRead),
// in kind and far-end order. A member that states no such link is in the map with none, and one
// whose address does not start with the document's ref_key is no block the pass addresses and is
// left out. A tuple of any other kind out of a block is no link the pass's pairs are compared
// with, since no block_refs row projects it: its membership and its placement, its values, and
// what another writer states of it - a requirement's constrained_by, a changed_by_run, a
// decision's or a reading's authored_by, a copy's copied_from. A gone block's pointers are
// retractGonePointers' to retract.
//
// It is the store's own side of the document, and IndexBlocks reads it only where that side is
// stale (standsAtVersion): another release indexed the document after the store last stated its
// blocks, and that release deleted the rows of the blocks and the links its version dropped and
// retracted nothing, so no row is left to say what the store still holds. The pass's input stays
// the version's render: this read is what the store would otherwise go on asserting against it,
// as heldBlockParentDifferences is for the parents. The members come through
// graph_tuples_inbound in one probe and each member's links through graph_tuples_current in one
// probe a block, in one statement. The kinds and the value kind are written into the text so the
// planner can prove each index's partial predicate whatever plan it caches.
func heldDocumentBlocks(ctx context.Context, tx pgx.Tx, refKey string) (map[string][]heldLink, error) {
	rows, err := tx.Query(ctx, `
		select member.id, held.kind, held.to_kind, held.to_id
		from (
			select distinct t.from_id as id from graph_tuples t
			where t.to_kind = 'artifact' and t.to_id = $1
			  and t.kind = '`+TupleInDocument+`' and t.from_kind = '`+BlockNodeKind+`'
		) member
		join graph_nodes n on n.kind = '`+BlockNodeKind+`' and n.id = member.id
			and n.state <> '`+graphstore.StateGone+`'
		left join lateral (
			select current.kind, current.to_kind, current.to_id
			from (
				`+graphstore.CurrentTupleRead(`t.kind, t.to_kind, t.to_id`, `t.kind, t.to_kind, t.to_id, t.asserted`,
		`t.from_kind = '`+BlockNodeKind+`' and t.from_id = member.id
				  and t.to_kind <> '`+graphstore.KindValue+`' and `+heldLinkKinds)+`
			) current
			where current.asserted
		) held on true
		order by member.id, held.kind, held.to_kind, held.to_id
	`, refKey)
	if err != nil {
		return nil, fmt.Errorf("read what the store states of the blocks of %s: %w", refKey, err)
	}
	defer rows.Close()
	prefix := refKey + "#"
	held := map[string][]heldLink{}
	for rows.Next() {
		var address string
		var kind, toKind, toID *string
		if err := rows.Scan(&address, &kind, &toKind, &toID); err != nil {
			return nil, fmt.Errorf("scan what the store states of a block of %s: %w", refKey, err)
		}
		id, ours := strings.CutPrefix(address, prefix)
		if !ours {
			continue
		}
		links := held[id]
		if kind != nil {
			links = append(links, heldLink{kind: *kind, toKind: *toKind, toID: *toID})
		}
		held[id] = links
	}
	return held, rows.Err()
}

// goneSinceStated is, in id order, each block the store holds present in the document (held) that
// the version holds no node of (nodes, its unique blocks) and that the rows did not hold either
// (deleted, the blocks of the rows the version dropped, which IndexBlocks states gone itself): a
// block another release's version dropped after the store last stated the document. That release
// deleted the block's row and stated nothing, so its node stands, and it is gone exactly as a
// block the rows dropped is, by the version that does not hold it.
func goneSinceStated(held map[string][]heldLink, nodes []model.ArtifactBlock, deleted []string) []string {
	if len(held) == 0 {
		return nil
	}
	holds := make(map[string]struct{}, len(nodes)+len(deleted))
	for _, block := range nodes {
		holds[block.ID] = struct{}{}
	}
	for _, id := range deleted {
		holds[id] = struct{}{}
	}
	var gone []string
	for id := range held {
		if _, kept := holds[id]; !kept {
			gone = append(gone, id)
		}
	}
	slices.Sort(gone)
	return gone
}

// unheldLinks retracts each link the store states now out of a block of the document (held) that
// no pair of the version writes, at the version's time and by its writer, as replaceBlockRefs
// retracts the pair of a row it drops: a link another release's version dropped after the store
// last stated the document, deleting its block_refs row and retracting nothing. The links of a
// block goneSinceStated names are among them, as a dropped block's rows are among the rows
// replaceBlockRefs drops. stated is the tuples replaceBlockRefs answered, and a link it already
// retracts, the pair of a row it dropped, is not retracted twice. The retractions come in block
// id order and in the read's order within a block, so a reader of the log can predict them.
func unheldLinks(held map[string][]heldLink, refKey string, pairs []blockPair, stated []graphstore.UncausedTuple, at time.Time, writer model.Actor) []graphstore.UncausedTuple {
	if len(held) == 0 {
		return nil
	}
	type blockLink struct {
		blockID string
		link    heldLink
	}
	kept := make(map[blockLink]struct{}, len(pairs)+len(stated))
	for _, pair := range pairs {
		kept[blockLink{pair.blockID, heldLink{pair.kind, pair.target.Kind, pair.target.ID}}] = struct{}{}
	}
	prefix := refKey + "#"
	for _, tuple := range stated {
		if id, ours := strings.CutPrefix(tuple.FromID, prefix); ours && !tuple.Asserted {
			kept[blockLink{id, heldLink{tuple.Kind, tuple.ToKind, tuple.ToID}}] = struct{}{}
		}
	}
	var retracted []graphstore.UncausedTuple
	for _, id := range slices.Sorted(maps.Keys(held)) {
		for _, link := range held[id] {
			if _, ok := kept[blockLink{id, link}]; ok {
				continue
			}
			retracted = append(retracted, graphstore.UncausedTuple{Tuple: graphstore.Tuple{
				FromKind: BlockNodeKind, FromID: blockNodeAddress(refKey, id), Kind: link.kind,
				ToKind: link.toKind, ToID: link.toID,
				At: at, Who: writer, Asserted: false,
			}})
		}
	}
	return retracted
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
