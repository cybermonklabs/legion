package refs

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// writtenAtAcceptance reports whether a block of type typ is one the transaction that accepts its
// version writes, of any size, where the tree it wrote renders as the version's markdown: a
// requirement, a check or a plan task. A decision and a reading are the
// typed-node pass's own (PlanDecisionNodes).
func writtenAtAcceptance(typ string) bool {
	switch typ {
	case requirementType, checkType, planTaskType:
		return true
	}
	return false
}

// PlanAuthored plans, beside the typed-node pass d over the same version and against the block
// index that pass leaves (DecisionIndexing.Held), what the write that leaves the version's other
// blocks to the block-index reconciliation writes of each requirement, check and plan task of the
// version (writtenAtAcceptance), so the coverage read and the tree read answer each one at once
// with no read changed: its block node where the store holds none live,
// under the supertag its writer gives (TypedFacts.supertag); its document_blocks row, with its
// type, its text_sha256 and its changed_version and the other columns as the block index writes
// them (BlockIndexing.queueRows), its text recorded in document_block_texts at that version, its
// text value, from whose first line a read takes the node's type (TreeNodeType), and, for a row
// it inserts, its membership of its document (in_document, dated from the row's created_at,
// now); its part_of, the parent its author gives it (planBlockParents over these blocks alone);
// for each serves pointer it holds, its block_refs row and its serves tuple (planBlockRefs over a
// scope of the serves rows these blocks hold); and, for a block of an issue room, the document
// whose fragment holds it in issue_room_blocks, as planRoomTypedBlocks records a decision's. It
// writes each block of the version their part_of climbs through up to the document's top - a
// criterion one names its parent, a list or a callout one stands in - and each the part_of of a
// decision or a reading the typed-node pass placed climbs through, in the same way and with no
// serves of its own, so every part_of the write states names a node the store holds.
//
// A block whose row the index holds with its text and its document unchanged keeps it, and
// states nothing of its node unless the store holds that node gone or under another supertag. It
// states no rank: document_blocks holds no position, and the typed-node pass ranks no decision
// either, so such a block holds no rank until the block-index reconciliation ranks it, and the
// walk orders it after its ranked siblings. Every other block of the version, and these blocks'
// ranks, placements and places in the text, wait for the reconciliation, which finds these nodes,
// values, tuples and rows standing and appends none of them again. record is what the newest of
// versions' rows says of the pass, holding the pass's own time (IndexedVersion.At), at which every
// value and tuple it states is dated; blocks index into markdown, located are the mentions of
// markdown (NewReader) and pointers its `block[]` values, as PlanBlocks takes them, and serverURL
// is the deployment the version's text was read against, whose dashboard URLs each block's
// excerpt is cut around (BlockExcerpt), as PlanBlocks cuts it (VersionBlocks.ServerURL). Every
// read is q's, in the write's read phase, and Queue writes it with the typed-node pass's, in one
// batch.
func (d DecisionIndexing) PlanAuthored(ctx context.Context, q Queryer, record IndexedVersion, versions Versions, markdown string, blocks []model.ArtifactBlock, located []text.Located, pointers []Pointer, serverURL string, now time.Time) (DecisionIndexing, error) {
	refKey := record.RefKey
	nodes, _, _ := uniqueBlocks(blocks)
	fits, _ := blockFits(refKey, nodes)
	// The blocks the pass writes: the version's requirements, checks and plan tasks, and every
	// block of the version their part_of climbs through up to the document's top, and that the
	// part_of of each decision and reading the typed-node pass placed (DecisionIndexing.blocks)
	// climbs through: a criterion one names its parent, a list or a callout one stands in. So no
	// part_of the write states names a node the store does not hold, and the walk from the issue
	// reaches each node it writes. A decision or a
	// reading is the typed-node pass's own node, which the pass climbs past and writes no row of.
	only := map[string]struct{}{}
	var accepted []string
	for position, block := range nodes {
		if fits[position] && writtenAtAcceptance(block.Type) {
			only[block.ID] = struct{}{}
			accepted = append(accepted, block.ID)
		}
	}
	types := make(map[string]string, len(nodes))
	for _, block := range nodes {
		types[block.ID] = block.Type
	}
	climbs := func(id string) bool {
		_, held := only[id]
		return !held && types[id] != "" && types[id] != decisionType && types[id] != readingType && blockIDFits(refKey, id)
	}
	for index := range d.blocks.placedIDs {
		if parent := d.blocks.placedParents[index]; parent != nil && climbs(*parent) {
			only[*parent] = struct{}{}
		}
	}
	if len(only) == 0 {
		return d, nil
	}
	if record.At.IsZero() {
		return DecisionIndexing{}, fmt.Errorf("plan the requirements, checks and plan tasks of %s: the pass holds no time of its own", d.artifactID)
	}
	index := d.held
	lead, newest := versions.Newest(d.artifactID)
	fragments, err := fragmentDocuments(ctx, q, d.artifactID, nodes)
	if err != nil {
		return DecisionIndexing{}, err
	}
	b := BlockIndexing{artifactID: d.artifactID, refKey: refKey, version: newest.Number, versionAt: newest.At, versions: versions}
	// A pass over one fragment of an issue room (a room left owed) covers one document's version:
	// every row of another document of the room that names its node stands outside it, a parent
	// its authored nodes may take (placeAuthored), so no part_of names a node the store lacks.
	for id, stored := range index.blocks {
		document := stored.document
		if document == "" {
			document = d.artifactID
		}
		if _, covered := versions[document]; covered || stored.retired || stored.node == "" {
			continue
		}
		if b.outside == nil {
			b.outside = map[string]struct{}{}
		}
		b.outside[id] = struct{}{}
	}
	// Each placement names the parent of each block it places where that parent is a block of the
	// version; one not placed yet joins the blocks the pass writes, and the blocks are placed again
	// until no part_of names such a block the pass does not write. An authored node takes a block of
	// the version as its parent only where the placement holds it (placeAuthored), so each block of
	// the version its parent attribute or its serves names joins first, as a pass over every block
	// would place it under that block. A chain climbs a block at a time, so the passes are as many
	// as its blocks of the version are deep, each one's reads in the write's read phase.
	sources := authoredSourcesOf(markdown, nodes, located, pointers)
	addressed := make([]string, 0, len(nodes))
	for position, node := range nodes {
		if fits[position] {
			addressed = append(addressed, blockNodeAddress(refKey, node.ID))
		}
	}
	var parents []graphstore.UncausedTuple
	var placed map[string]blockFarEnd
	for {
		climbed := false
		var authors []string
		for id := range only {
			if _, named := sources.parent[id]; named || len(sources.serves[id]) > 0 {
				authors = append(authors, id)
			}
		}
		if len(authors) > 0 {
			slices.Sort(authors)
			farEnds, err := authoredFarEnds(ctx, q, d.artifactID, addressed, authors, sources)
			if err != nil {
				return DecisionIndexing{}, err
			}
			for _, id := range authors {
				named := sources.serves[id]
				if parent, held := sources.parent[id]; held {
					named = append([]text.Ref{parent}, named...)
				}
				for _, ref := range named {
					target, ok := TargetOf(ref)
					if !ok {
						continue
					}
					if far := resolvedFarEnd(target, farEnds); far.kind == BlockNodeKind {
						if own, cut := strings.CutPrefix(far.id, refKey+"#"); cut && climbs(own) {
							only[own], climbed = struct{}{}, true
						}
					}
				}
			}
		}
		if climbed {
			continue
		}
		placement := b
		var stated []graphstore.UncausedTuple
		var standing map[string]blockFarEnd
		if _, stated, standing, err = placement.planBlockParents(ctx, q, index, record, nodes, fits, fragments, askPlacements{}, only, sources); err != nil {
			return DecisionIndexing{}, err
		}
		for at := range placement.placedIDs {
			if parent := placement.placedParents[at]; parent != nil && climbs(*parent) {
				only[*parent], climbed = struct{}{}, true
			}
		}
		if !climbed {
			b.placedIDs, b.placedParents, parents, placed = placement.placedIDs, placement.placedParents, stated, standing
			break
		}
	}
	var positions []int
	for position, block := range nodes {
		if _, written := only[block.ID]; written && fits[position] {
			positions = append(positions, position)
		}
	}
	ids := make([]string, len(positions))
	addresses := make([]string, len(positions))
	// The document each block is a block of, as PlanBlocks takes it: the one its row names, else
	// the one whose fragment of the issue room holds it, else the indexed document itself.
	documents := make([]string, len(positions))
	for at, position := range positions {
		block := nodes[position]
		ids[at], addresses[at] = block.ID, blockNodeAddress(refKey, block.ID)
		documents[at] = d.artifactID
		if stored, held := index.blocks[block.ID]; held && stored.document != "" {
			documents[at] = stored.document
		} else if _, known := fragments[block.Document]; known {
			documents[at] = block.Document
		}
		if documents[at] != d.artifactID {
			if b.documentOf == nil {
				b.documentOf = map[string]string{}
			}
			b.documentOf[block.ID] = documents[at]
		}
	}
	heldNodes, err := heldBlockNodes(ctx, q, addresses)
	if err != nil {
		return DecisionIndexing{}, fmt.Errorf("read the nodes of the requirements, checks and plan tasks of %s: %w", refKey, err)
	}
	// The blocks whose row the pass writes: one the index holds no row of, which the pass inserts
	// and which joins its document there, or one whose text or document moved.
	var sum blockSumHex
	changed := make([]bool, len(positions))
	inserted := make([]bool, len(positions))
	held := maps.Clone(index.blocks)
	if held == nil {
		held = make(map[string]storedBlock, len(positions))
	}
	for at, position := range positions {
		block := nodes[position]
		body := markdown[block.From:block.To]
		blockTextSum(&sum, body)
		stored, kept := index.blocks[block.ID]
		was := stored.document
		if was == "" {
			was = d.artifactID
		}
		if kept && stored.sum == string(sum[:]) && was == documents[at] {
			continue
		}
		hexSum := string(sum[:])
		joined := now
		if kept {
			joined = stored.joined
		}
		held[block.ID] = storedBlock{sum: hexSum, joined: joined, node: addresses[at], supertag: TypeSupertag(block.Type), document: documents[at]}
		changed[at], inserted[at] = true, !kept
		version := b.versionOf(block.ID)
		b.ids, b.types, b.sums = append(b.ids, block.ID), append(b.types, block.Type), append(b.sums, hexSum)
		b.excerpts = append(b.excerpts, BlockExcerpt(body, serverURL))
		b.since, b.sinceAt = append(b.since, int32(version.Number)), append(b.sinceAt, version.At)
		b.textIDs, b.texts = append(b.textIDs, block.ID), append(b.texts, &body)
	}
	b.textVersions = b.since
	// Who made each block whose row the pass writes: the first author of the version its row
	// records (versionAuthors), as PlanBlocks makes a block.
	authors := map[documentVersion][]model.Actor{{document: lead, number: newest.Number}: record.Authors}
	if len(b.ids) > 0 {
		wanted := make([]documentVersion, 0, len(b.ids))
		for position, id := range b.ids {
			wanted = append(wanted, documentVersion{document: b.versionDocument(id), number: int(b.since[position])})
		}
		if authors, err = versionAuthors(ctx, q, authors, wanted); err != nil {
			return DecisionIndexing{}, err
		}
	}
	// The nodes: a block whose row the pass writes, marked changed as the block index marks a
	// block whose words a version wrote, and a block whose row stands but whose node the store
	// holds gone, under another supertag or not at all. A node the typed-node pass writes already
	// is not written twice, since one write of the store takes each node once.
	written := make(map[string]struct{}, len(d.stated.Nodes))
	for _, node := range d.stated.Nodes {
		if node.Kind == BlockNodeKind {
			written[node.ID] = struct{}{}
		}
	}
	var stated []graphstore.Node
	var tuples []graphstore.UncausedTuple
	changes := 0
	for at, position := range positions {
		block, address := nodes[position], addresses[at]
		supertag := TypedFacts{}.supertag(block.ID, block.Type)
		if _, already := written[address]; !already {
			switch node, holds := heldNodes[address]; {
			case changed[at]:
				var maker model.Actor
				if made := authors[documentVersion{document: b.versionDocument(block.ID), number: int(b.since[changes])}]; len(made) > 0 {
					maker = made[0]
				}
				stated = append(stated, blockNode(address, record, maker, b.sinceAt[changes], graphstore.StateLive, supertag, true))
			case !holds || node.state == graphstore.StateGone || node.supertag != supertag:
				stated = append(stated, blockNode(address, record, record.Writer, b.versionOf(block.ID).At, graphstore.StateLive, supertag, false))
			}
		}
		if !changed[at] {
			continue
		}
		changes++
		// The text value and the document_block_texts row move together (planBlockValues), and a
		// row the pass inserts states the membership the insert projects (PlanBlocks).
		tuples = append(tuples, graphstore.UncausedTuple{Tuple: graphstore.Tuple{
			FromKind: BlockNodeKind, FromID: address, Kind: graphstore.AttributeText,
			ToKind: graphstore.KindValue, ToID: markdown[block.From:block.To],
			At: record.At, Who: record.Writer, Asserted: true,
		}})
		// A row the pass inserts is of a block no pass has placed yet: it takes what decides its
		// parent, as planBlockValues states it, so the store census reads an authored node's part_of
		// to no block of its artifact as its row's null parent states it.
		if inserted[at] {
			placement := PlacementText
			if authoredType(block.Type) {
				placement = PlacementAuthored
			}
			tuples = append(tuples, membership(address, record.inDocument(block, fragments), now, record.Writer),
				graphstore.UncausedTuple{Tuple: graphstore.Tuple{
					FromKind: BlockNodeKind, FromID: address, Kind: AttributePlacement,
					ToKind: graphstore.KindValue, ToID: placement,
					At: record.At, Who: record.Writer, Asserted: true,
				}})
		}
	}
	tuples = append(tuples, parents...)
	// The serves pairs of these blocks, their far ends resolved as PlanBlocks resolves them, the
	// blocks of the version written as the full pass writes them; the scope is the serves rows these
	// blocks hold, so no other row of the document moves.
	var serves []Pointer
	for _, pointer := range pointers {
		if pointer.Kind == servesKind && writtenAtAcceptance(pointer.BlockType) {
			serves = append(serves, pointer)
		}
	}
	if paired, _ := pointerPairs(nodes, serves); len(paired) > 0 {
		if b.pairs, err = resolvePointerPairs(ctx, q, d.artifactID, addressed, paired); err != nil {
			return DecisionIndexing{}, err
		}
	}
	slices.Sort(accepted)
	b.scope = &pairScope{kinds: []string{servesKind}, blocks: accepted}
	links, heldLinks, err := b.planBlockRefs(ctx, q, index, record.Writer)
	if err != nil {
		return DecisionIndexing{}, err
	}
	tuples = append(tuples, links...)
	plan := d
	plan.authored = b
	// An issue room records the document of each block it places, which every reader of a block's
	// document reads (planRoomTypedBlocks); a document that is its own room records nothing.
	if record.Band == RankBandTree {
		for at, id := range ids {
			plan.roomIDs, plan.roomDocuments = append(plan.roomIDs, id), append(plan.roomDocuments, documents[at])
		}
	}
	plan.stated = graphstore.Joined(d.stated, graphstore.PlanUncaused(stated, nil))
	plan.stated.Tuples = append(plan.stated.Tuples, tuples...)
	plan.held.blocks, plan.held.links, plan.held.placed, plan.held.rooms = held, heldLinks, placed, plan.heldRooms(index)
	return plan, nil
}

// versionDocument is the document whose version block blockID's row records (versionOf): the one
// whose fragment of an issue room holds it where the pass covers that document, else the indexed
// document where the pass covers it, else the document of the newest version the pass covers.
func (b BlockIndexing) versionDocument(blockID string) string {
	if document, held := b.documentOf[blockID]; held {
		if _, covered := b.versions[document]; covered {
			return document
		}
	}
	if _, covered := b.versions[b.artifactID]; covered {
		return b.artifactID
	}
	lead, _ := b.versions.Newest(b.artifactID)
	return lead
}

// heldBlockNodes reads, in one statement, the supertag and the state of each block node of
// addresses the store holds, by address.
func heldBlockNodes(ctx context.Context, q Queryer, addresses []string) (map[string]heldNode, error) {
	rows, err := q.Query(ctx, `select id, supertag, state from graph_nodes where kind = $1 and id = any($2::text[])`, BlockNodeKind, addresses)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	held := make(map[string]heldNode, len(addresses))
	for rows.Next() {
		var address string
		var node heldNode
		if err := rows.Scan(&address, &node.supertag, &node.state); err != nil {
			return nil, err
		}
		held[address] = node
	}
	return held, rows.Err()
}
