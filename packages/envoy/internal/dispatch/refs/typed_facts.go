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

// supertag is the supertag of the node of block id, of type typ: its own type's for a decision
// and for a reading the facts hold to be one, and BlockSupertag for every other block. Facts that
// read nothing tag every decision and reading by its type.
func (f TypedFacts) supertag(id, typ string) string {
	if f.read {
		if supertag, typed := f.supertags[id]; typed {
			return supertag
		}
		return BlockSupertag
	}
	switch typ {
	case decisionType, readingType:
		return typ
	}
	return BlockSupertag
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
// decided_by (pairs).
func (f TypedFacts) typedPairs() []blockPair {
	paired, _ := pointerPairs(f.nodes, slices.Concat(f.shaped, f.ties))
	return f.pairs(paired)
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
func (s *pairScope) holding(index BlockIndex) *pairScope {
	blocks := slices.Clone(s.blocks)
	for _, link := range index.links {
		if slices.Contains(s.kinds, link.kind) {
			blocks = append(blocks, link.blockID)
		}
	}
	slices.Sort(blocks)
	return &pairScope{kinds: s.kinds, blocks: slices.Compact(blocks)}
}

// indexedFacts is what the typed-node pass states of the version beside the block index that
// indexes it in the same write (PlanBlocks), which writes the node of every block whose text the
// version moved, states each typed node's part_of, reconciles every block_refs row of the
// document with the facts' ties among them, and states gone each block the version no longer
// holds. So the pass adds only the node of a decision or a reading the store holds otherwise than
// the version leaves it - under another supertag (a block retyped into a decision, a reading
// whose ask now holds an answer) or gone - with its type's supertag, the same node the block index
// writes (blockNode); a block the store holds no node of is one whose text this version wrote,
// and the block index writes its node. So a version that moves one word of a document of many
// decisions writes no node of its own. Beside those it states each author and each decision's
// values (typedValues). written is the nodes the block index writes, none of which the pass
// writes again: one write of the store takes each node once.
func (f TypedFacts) indexedFacts(versionAt time.Time, written []graphstore.Node) ([]graphstore.Node, []graphstore.Tuple) {
	if !f.read {
		return nil, nil
	}
	refKey := f.document.RefKey
	writes := make(map[string]struct{}, len(written))
	for _, node := range written {
		writes[node.ID] = struct{}{}
	}
	var nodes []graphstore.Node
	for _, id := range f.typed {
		if !blockIDFits(refKey, id) {
			continue
		}
		address := blockNodeAddress(refKey, id)
		held, holds := f.heldNodes[address]
		if !holds || held.supertag == f.supertags[id] && held.state == graphstore.StateLive {
			continue
		}
		if _, indexed := writes[address]; indexed {
			continue
		}
		nodes = append(nodes, blockNode(address, f.document, versionAt, graphstore.StateLive, f.supertags[id]))
	}
	authors, tuples := f.typedValues(versionAt)
	return append(nodes, authors...), tuples
}

// typedValues is what the typed-node pass states of the version's decisions and readings beyond
// their nodes: of a decision, uncertainty, uncertainty_reason and replaced as values where they
// moved, and a retraction of one the decision no longer holds, so a decision whose author recorded
// neither level nor reason holds neither; and of a decision or a reading that holds no author, the
// node of the person or session who introduced the block (TypedFacts.authored, through
// graphstore.AuthorFacts) and the aliases the server proved of it, the far end of a decision's
// decided_by and a reading's authored_by. A decision's tie itself is its block_refs row's, stated
// by the reconcile that writes the row (planBlockRefs), and a reading's authored_by is stated
// here, both once. It answers the author nodes and the tuples.
func (f TypedFacts) typedValues(versionAt time.Time) ([]graphstore.Node, []graphstore.Tuple) {
	refKey, writer := f.document.RefKey, f.document.Writer
	var nodes []graphstore.Node
	var tuples []graphstore.Tuple
	// Each author's node and the aliases the server proved of it are stated once however many
	// blocks name it, since one write of the store takes each node once.
	authors := map[Target]struct{}{}
	author := func(address string, actor model.Actor) []graphstore.Tuple {
		authorNodes, stated, named := graphstore.AuthorFacts(BlockNodeKind, address, actor, versionAt, graphstore.NoEvent)
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
		if actor, authored := f.authored[decision.id]; authored {
			author(address, actor)
		}
		held := f.held[address]
		for index, kind := range decisionValues {
			stored, value := held[kind], decision.values[index]
			switch {
			case value != nil && (!stored.asserted || stored.toID != *value):
				tuples = append(tuples, valueTuple(address, kind, *value, versionAt, writer, true))
			case value == nil && stored.asserted:
				tuples = append(tuples, valueTuple(address, kind, stored.toID, versionAt, writer, false))
			}
		}
	}
	for _, reading := range f.readings {
		if actor, authored := f.authored[reading]; authored && blockIDFits(refKey, reading) {
			tuples = append(tuples, author(blockNodeAddress(refKey, reading), actor)...)
		}
	}
	return nodes, tuples
}
