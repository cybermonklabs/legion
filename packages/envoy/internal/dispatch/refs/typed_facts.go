package refs

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// readHeld reads what the store holds of each typed node that fits its key, in one statement:
// the node's supertag and state (heldNodes), and the newest tuple of each value it holds and of
// its decided_by and its authored_by (held). It then names the author of each decision and
// reading that holds none from sources (authored), and each decision's decided_by (decidedBy).
func (f *TypedFacts) readHeld(ctx context.Context, q Queryer, sources TypedSources) error {
	refKey := f.document.RefKey
	addresses := make([]string, 0, len(f.typed))
	for _, id := range f.typed {
		if blockIDFits(refKey, id) {
			addresses = append(addresses, blockNodeAddress(refKey, id))
		}
	}
	f.held = make(map[string]map[string]heldTie, len(addresses))
	f.heldNodes = make(map[string]heldNode, len(addresses))
	if len(addresses) > 0 {
		kinds := append([]string{graphstore.TupleDecidedBy, graphstore.TupleAuthoredBy}, decisionValues[:]...)
		rows, err := q.Query(ctx, `
			select typed.address, n.supertag, n.state, t.kind, t.to_kind, t.to_id, t.asserted
			from unnest($2::text[]) as typed (address)
			left join graph_nodes n on n.kind = $1 and n.id = typed.address
			left join lateral (
				select distinct on (h.kind) h.kind, h.to_kind, h.to_id, h.asserted
				from graph_tuples h
				where h.from_kind = $1 and h.from_id = typed.address and h.kind = any($3::text[])
				order by h.kind, h.seq desc
			) t on true
		`, BlockNodeKind, addresses, kinds)
		if err != nil {
			return fmt.Errorf("read what the store holds of the decisions and readings of %s: %w", refKey, err)
		}
		defer rows.Close()
		for rows.Next() {
			var address string
			var supertag, state, kind, toKind, toID *string
			var asserted *bool
			if err := rows.Scan(&address, &supertag, &state, &kind, &toKind, &toID, &asserted); err != nil {
				return fmt.Errorf("scan what the store holds of a decision or a reading of %s: %w", refKey, err)
			}
			if supertag != nil && state != nil {
				f.heldNodes[address] = heldNode{supertag: *supertag, state: *state}
			}
			if kind == nil {
				continue
			}
			if f.held[address] == nil {
				f.held[address] = map[string]heldTie{}
			}
			f.held[address][*kind] = heldTie{toKind: *toKind, toID: *toID, asserted: *asserted}
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("read what the store holds of the decisions and readings of %s: %w", refKey, err)
		}
	}
	// A node holding its author keeps it: the author a block takes is the one of the write that
	// introduced it, and only a node that holds none yet is given one.
	var unauthored []string
	for _, decision := range f.decisions {
		if !f.serverDecided || !blockIDFits(refKey, decision.id) {
			continue
		}
		if held, stated := f.held[blockNodeAddress(refKey, decision.id)][graphstore.TupleDecidedBy]; stated {
			f.setDecidedBy(decision.id, Target{Kind: held.toKind, ID: held.toID})
		} else {
			unauthored = append(unauthored, decision.id)
		}
	}
	for _, reading := range f.readings {
		if !blockIDFits(refKey, reading) {
			continue
		}
		if _, stated := f.held[blockNodeAddress(refKey, reading)][graphstore.TupleAuthoredBy]; !stated {
			unauthored = append(unauthored, reading)
		}
	}
	if len(unauthored) == 0 || sources.Authors == nil {
		return nil
	}
	authors, err := sources.Authors(unauthored)
	if err != nil {
		return fmt.Errorf("read who introduced the decisions and readings of %s: %w", refKey, err)
	}
	for _, id := range unauthored {
		actor, named := authors[id]
		kind, authorID, proved := graphstore.AuthorOf(actor)
		if !named || !proved || !graphstore.AddressFits(authorID) {
			continue
		}
		if f.authored == nil {
			f.authored = map[string]model.Actor{}
		}
		f.authored[id] = actor
		if f.supertags[id] == decisionType {
			f.setDecidedBy(id, Target{Kind: kind, ID: authorID})
		}
	}
	return nil
}

func (f *TypedFacts) setDecidedBy(id string, author Target) {
	if f.decidedBy == nil {
		f.decidedBy = map[string]Target{}
	}
	f.decidedBy[id] = author
}

// supertag is the supertag of the node of block id, of type typ (R26 of the issue-tree
// spec): a decision's and a reading's are the facts' to decide - each decision, and each reading but
// one in an ask a person has not answered or in no decision or ask, takes its type's, and every
// other one BlockSupertag - and every other block's is its type's (TypeSupertag): its type for a
// typed block of the block schema, BlockSupertag for a paragraph, a heading, a list or any other
// kind. Facts that read nothing tag every decision and reading by its type. So every writer of a
// block node gives one block one supertag: the block index and the typed-node pass this one, and
// the requirements, checks and plan tasks a deferred version's write writes (PlanAuthored) the one
// TypeSupertag gives, which is this one for those types.
func (f TypedFacts) supertag(id, typ string) string {
	if f.read && (typ == decisionType || typ == readingType) {
		if supertag, typed := f.supertags[id]; typed {
			return supertag
		}
		return BlockSupertag
	}
	return TypeSupertag(typ)
}

// decides says the facts decide the supertag of a block of type typ: a decision's and a reading's
// where they read the version's tree, and every other type's always (supertag). Facts that read
// nothing - a version whose tree did not read back - tag a decision and a reading by its type,
// which a reading in an ask nobody answered is not, so the block index leaves the node of an
// unchanged decision or reading as it stands under such facts.
func (f TypedFacts) decides(typ string) bool {
	return f.read || f.bare || (typ != decisionType && typ != readingType)
}

// asks is the blocks of the version that stand under an ask's node, as planBlockParents places
// them: each reading in an answered ask (askParents) and each decision that settles an ask of its
// issue (settles).
func (f TypedFacts) asks() askPlacements {
	return askPlacements{readings: f.askParents, decisions: f.settles}
}

// withTies is pointers with each decision's written ties beside them (a from_ask), which the
// block index pairs as it pairs a pointer: a from_ask value is written in its decision's opening
// line, so the mention the extraction finds there is the tie's and no mention of its own
// (pointerPairs).
func (f TypedFacts) withTies(pointers []Pointer) []Pointer {
	if len(f.ties) == 0 {
		return pointers
	}
	return slices.Concat(pointers, f.ties)
}

// pairs is a version's pairs (blockPairs) as planBlockRefs reconciles them: a tie naming
// another issue left out (foreign), an ask block's shaped marked with its ask's node or, where no
// row names that node yet, left out, and each decision's decided_by added, its far end the author
// decidedBy names. The typed-node pass hands the same function the pairs of its own ties, so the
// rows it writes above the deferral bound are the rows the block index writes for one version.
func (f TypedFacts) pairs(pairs []blockPair) []blockPair {
	if !f.read {
		return pairs
	}
	kept := make([]blockPair, 0, len(pairs)+len(f.decidedBy))
	for _, pair := range pairs {
		if _, foreign := f.foreign[tieKey{block: pair.blockID, kind: pair.kind, target: pair.target}]; foreign {
			continue
		}
		if pair.kind == TupleShaped {
			if ask, onAsk := f.askBlocks[pair.blockID]; onAsk {
				if ask == "" {
					continue
				}
				pair.nodeKind, pair.nodeAddress = graphstore.KindAsk, ask
			}
		}
		kept = append(kept, pair)
	}
	for _, decision := range f.decisions {
		if author, decided := f.decidedBy[decision.id]; decided {
			kept = append(kept, blockPair{blockID: decision.id, kind: graphstore.TupleDecidedBy, target: author})
		}
	}
	return kept
}

// typedPairs is the pairs of the facts' own ties: each decision's and ask block's shaped and each
// decision's from_ask, paired as the block index pairs a pointer (pointerPairs), and its
// decided_by (pairs), each pointer's far end resolved, through q, to the node it resolves to now,
// as PlanBlocks resolves the pairs it reconciles (resolvePointerPairs): a `dispatch://KEY/spec#b-…`
// tie names the block of the spec's issue room once the spec is a fragment of that room (T17 of
// the issue-tree plan), and the blocks the facts were read of, at their addresses under the
// document artifactID the pass indexes, are the ones a pass writes.
func (f TypedFacts) typedPairs(ctx context.Context, q Queryer, artifactID string) ([]blockPair, error) {
	paired, _ := pointerPairs(f.nodes, slices.Concat(f.shaped, f.ties))
	written := make([]string, 0, len(f.nodes))
	for position, node := range f.nodes {
		if f.fits[position] {
			written = append(written, blockNodeAddress(f.document.RefKey, node.ID))
		}
	}
	return resolvePointerPairs(ctx, q, artifactID, written, f.pairs(paired))
}

// typedScope is the rows of block_refs the facts' own ties are: those of typedKinds held by the
// version's decisions and ask blocks.
func (f TypedFacts) typedScope() *pairScope {
	blocks := make([]string, 0, len(f.decisions)+len(f.askBlocks))
	for _, decision := range f.decisions {
		blocks = append(blocks, decision.id)
	}
	for id := range f.askBlocks {
		blocks = append(blocks, id)
	}
	slices.Sort(blocks)
	return &pairScope{kinds: typedKinds, blocks: blocks}
}

// holding is s widened to every block whose rows of s's kinds index holds. A block the version
// removed, or holds under a type whose ties are none of those kinds - a decision or an ask block
// taken out or retyped into a paragraph - holds no pair of them any more, so a reconcile of the
// widened scope drops its rows and retracts each one's tuple out of the node the row names, the
// ask's for an ask block's shaped, as the block index's reconcile of every row would.
//
// Two kinds of block are left out, each by what the pass knows of its place (tieBlock): the
// index's document_blocks row of it, else the issue room's record of it (unrowedTieBlocks). A block
// of a retired document of an issue room is retained on purpose: no fragment of the room holds it
// now, and every pass over the room keeps its row, its links and its node as the retirement left
// them (BlockIndexing.retained), so its ties are none of this version's to drop. And where fragment
// is not "", the pass states that one fragment of the room (PlanDecisionNodes' fragment) against
// the index of the whole room, so a block of any other fragment, or one neither the index nor the
// room's record places in any, is no block whose ties the version read, and it stands as it is
// until the room's own pass. A decision an earlier pass over the same fragment placed, whose row
// only the room's own pass writes, is the fragment's by the room's record, and its ties are the
// version's to drop like any other block's of it.
func (s *pairScope) holding(index BlockIndex, unrowed map[string]storedBlock, fragment string) *pairScope {
	blocks := slices.Clone(s.blocks)
	for _, link := range index.links {
		if !slices.Contains(s.kinds, link.kind) {
			continue
		}
		stored, held := tieBlock(index, unrowed, link.blockID)
		if stored.retired || fragment != "" && (!held || stored.document != fragment) {
			continue
		}
		blocks = append(blocks, link.blockID)
	}
	slices.Sort(blocks)
	return &pairScope{kinds: s.kinds, blocks: slices.Compact(blocks)}
}

// tieBlock is what the typed-node pass knows of the place of block id, whose ties index holds: its
// document_blocks row in index, else what the room's record holds of it (unrowed), and false where
// neither holds it.
func tieBlock(index BlockIndex, unrowed map[string]storedBlock, id string) (storedBlock, bool) {
	if stored, held := index.blocks[id]; held {
		return stored, true
	}
	stored, recorded := unrowed[id]
	return stored, recorded
}

// unrowedTieBlocks is what the record of the issue room artifactID (issue_room_blocks) says of each
// block whose rows of kinds index holds and of which the index holds no document_blocks row: the
// document whose fragment holds it and whether that document is retired, by block id, as
// storedBlocks reads a row's. Such a block is a decision or an ask block a pass of the typed-node
// pass alone placed (PlanDecisionNodes), whose row only the room's own pass writes, and which that
// pass recorded in the room all the same (planRoomTypedBlocks). The record is read through q, in the
// plan's read phase; a block the table names none of takes the document index records for it
// (BlockIndex.rooms), as a pass of the reading transaction queued it, which writes no row over one
// the table holds (queueRoomTypedBlocks). A block no record names is absent, and a document that is
// no issue room (band) records nothing, so it reads nothing.
func unrowedTieBlocks(ctx context.Context, q Queryer, index BlockIndex, artifactID string, band byte, kinds []string) (map[string]storedBlock, error) {
	if band != RankBandTree {
		return nil, nil
	}
	var ids []string
	for _, link := range index.links {
		if _, held := index.blocks[link.blockID]; !held && slices.Contains(kinds, link.kind) {
			ids = append(ids, link.blockID)
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	slices.Sort(ids)
	ids = slices.Compact(ids)
	rows, err := q.Query(ctx, `
		select ids.block_id, room_block.document_id::text,
		       room_block.document_id <> $1::uuid and document.retired_at is not null
		from unnest($2::text[]) as ids (block_id)
		join issue_room_blocks room_block on room_block.tree_artifact_id = $1::uuid
		 and block_id_key(room_block.block_id) = block_id_key(ids.block_id) and room_block.block_id = ids.block_id
		join artifacts document on document.id = room_block.document_id
	`, artifactID, ids)
	if err != nil {
		return nil, fmt.Errorf("read the room's record of the typed blocks of %s that hold no row: %w", artifactID, err)
	}
	defer rows.Close()
	unrowed := make(map[string]storedBlock, len(ids))
	for rows.Next() {
		var id string
		var block storedBlock
		if err := rows.Scan(&id, &block.document, &block.retired); err != nil {
			return nil, fmt.Errorf("scan the room's record of a typed block of %s: %w", artifactID, err)
		}
		unrowed[id] = block
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the room's record of the typed blocks of %s that hold no row: %w", artifactID, err)
	}
	for _, id := range ids {
		if _, recorded := unrowed[id]; !recorded {
			if document, queued := index.rooms[id]; queued {
				unrowed[id] = storedBlock{document: document}
			}
		}
	}
	return unrowed, nil
}

// indexedFacts is what the typed-node pass states of the version beside the block index that
// indexes it in the same write (PlanBlocks), which writes the node of every block whose text the
// version moved, states each typed node's part_of, reconciles every block_refs row of the
// document with the facts' ties among them, and states gone each block the version no longer
// holds. So the pass adds only the node of a decision or a reading the store holds otherwise than
// the version leaves it - under another supertag (a block retyped into a decision, a reading
// whose ask now holds an answer) or gone - with its type's supertag, the same node the block index
// writes (blockNode); a block the store holds no node of is one whose text this version wrote,
// and the block index writes its node. A node the store holds at any state but gone - proposed,
// settled or overtaken as much as live - is present, and the pass leaves it. So a version that
// moves one word of a document of many decisions writes no node of its own. Beside those it
// states each author and each decision's values (typedValues). Each fact of a block is dated at
// the version of that block's own document (TypedFacts.versionOf): the one version of a document
// that is its own room, or the version of the document whose fragment of an issue room holds the
// block. written is the nodes the block index writes, none of which the pass writes again: one
// write of the store takes each node once.
func (f TypedFacts) indexedFacts(written []graphstore.Node) ([]graphstore.Node, []graphstore.Tuple) {
	if !f.read {
		return nil, nil
	}
	refKey := f.document.RefKey
	writes := make(map[Target]struct{}, len(written))
	for _, node := range written {
		writes[Target{Kind: node.Kind, ID: node.ID}] = struct{}{}
	}
	var nodes []graphstore.Node
	for _, id := range f.typed {
		if !blockIDFits(refKey, id) {
			continue
		}
		address := blockNodeAddress(refKey, id)
		held, holds := f.heldNodes[address]
		if !holds || held.supertag == f.supertags[id] && held.state != graphstore.StateGone {
			continue
		}
		if _, indexed := writes[Target{Kind: BlockNodeKind, ID: address}]; indexed {
			continue
		}
		// The typed pass moves a node's supertag or state and no block's words, so it marks no
		// node changed (blockNode): the block index marks each block whose words the version
		// moved.
		nodes = append(nodes, blockNode(address, f.document, f.document.Writer, f.versionOf(id).At, graphstore.StateLive, f.supertags[id], false))
	}
	// An author the block index states already (blockAuthors' nodes among written) is no node
	// this pass writes again.
	authors, tuples := f.typedValues()
	for _, author := range authors {
		if _, indexed := writes[Target{Kind: author.Kind, ID: author.ID}]; !indexed {
			nodes = append(nodes, author)
		}
	}
	return nodes, tuples
}

// typedValues is what the typed-node pass states of the version's decisions and readings beyond
// their nodes: of a decision, uncertainty, uncertainty_reason and replaced as values where they
// moved, and a retraction of one the decision no longer holds, so a decision whose author recorded
// neither level nor reason holds neither; and of a decision or a reading that holds no author, the
// node of the person or session who introduced the block (TypedFacts.authored, through
// graphstore.AuthorFacts) and the aliases the server proved of it, the far end of a decision's
// decided_by and a reading's authored_by. A decision's tie itself is its block_refs row's, stated
// by the reconcile that writes the row (planBlockRefs), and a reading's authored_by is stated
// here, both once. Each is dated at the version of its block's own document (versionOf). It
// answers the author nodes and the tuples.
func (f TypedFacts) typedValues() ([]graphstore.Node, []graphstore.Tuple) {
	refKey, writer := f.document.RefKey, f.document.Writer
	var nodes []graphstore.Node
	var tuples []graphstore.Tuple
	// Each author's node and the aliases the server proved of it are stated once however many
	// blocks name it, since one write of the store takes each node once.
	authors := map[Target]struct{}{}
	author := func(address string, actor model.Actor, at time.Time) []graphstore.Tuple {
		authorNodes, stated, named := graphstore.AuthorFacts(BlockNodeKind, address, actor, at, graphstore.NoEvent)
		if !named {
			return nil
		}
		key := Target{Kind: authorNodes[0].Kind, ID: authorNodes[0].ID}
		if _, written := authors[key]; !written {
			authors[key] = struct{}{}
			nodes = append(nodes, authorNodes...)
			tuples = append(tuples, stated[1:]...)
		}
		return stated[:1]
	}
	for _, decision := range f.decisions {
		if !blockIDFits(refKey, decision.id) {
			continue
		}
		address := blockNodeAddress(refKey, decision.id)
		at := f.versionOf(decision.id).At
		if actor, authored := f.authored[decision.id]; authored {
			author(address, actor, at)
		}
		held := f.held[address]
		for index, kind := range decisionValues {
			stored, value := held[kind], decision.values[index]
			switch {
			case value != nil && (!stored.asserted || stored.toID != *value):
				tuples = append(tuples, valueTuple(address, kind, *value, at, writer, true))
			case value == nil && stored.asserted:
				tuples = append(tuples, valueTuple(address, kind, stored.toID, at, writer, false))
			}
		}
	}
	for _, reading := range f.readings {
		if actor, authored := f.authored[reading]; authored && blockIDFits(refKey, reading) {
			tuples = append(tuples, author(blockNodeAddress(refKey, reading), actor, f.versionOf(reading).At)...)
		}
	}
	return nodes, tuples
}
