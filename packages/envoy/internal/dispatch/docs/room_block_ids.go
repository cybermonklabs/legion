package docs

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// roomBlockHolders is, for each block id a fragment of an issue room holds beside the one a write
// addresses, the document whose fragment holds it: the room itself for the tree artifact's own
// fragment (roomLayout.documents).
type roomBlockHolders map[string]string

// otherFragmentHolders reads, from room - the Yjs document of the issue room id, which only the
// caller reads: a write's fork, or the room's stored state a seed decoded (refuseUndecodableSeed)
// - every block id the fragments of layout hold but own, each with the document whose fragment
// holds it, the first holder in the room's order where two hold one. It reads each fragment's ids
// alone (blockIDsUnder) and no text, so a write into a room of large documents pays for their
// ids, not for their words.
func otherFragmentHolders(room *crdt.Doc, id string, layout roomLayout, own string) roomBlockHolders {
	holders := roomBlockHolders{}
	documents := layout.documents(id)
	var ids []string
	for index, fragment := range layout.fragments() {
		if fragment == own {
			continue
		}
		ids = blockIDsUnder(room.GetXmlFragment(fragment), ids[:0])
		for _, held := range ids {
			if _, first := holders[held]; !first {
				holders[held] = documents[index]
			}
		}
	}
	return holders
}

// forkBlockHolders is otherFragmentHolders for a write of artifactID's document on doc, its write's
// fork, which holds every fragment of the document's room as the write read it (forkLive), browser
// edits no settlement has versioned among them. A document that is its own room, and the tree
// artifact's own fragment, write no fragment of another document's room, so they read nothing and
// answer none. It reads the fork, so its caller calls it outside a Yjs transaction of the fork.
func (s *Service) forkBlockHolders(ctx context.Context, artifactID string, doc liveDoc) (roomBlockHolders, error) {
	if doc.fragment == fragmentName {
		return nil, nil
	}
	// The write's operation resolved the document into its room, which the transaction holds
	// (Ledger.noteRoom), so this resolution reads nothing.
	target, err := s.targetOf(ctx, artifactID)
	if err != nil {
		return nil, err
	}
	layout, err := readRoomLayout(ctx, s.queryFrom(ctx), target.room)
	if err != nil {
		return nil, err
	}
	return otherFragmentHolders(doc.Doc, target.room, layout, target.fragment), nil
}

// refuseForkBlockIDsOfAnotherDocument is refuseBlockIDsOfAnotherDocument for a write of
// artifactID's document on doc, its write's fork (forkBlockHolders): a replacement (ReplaceText)
// and an accepted suggestion (applySuggestion). written is the tree the write leaves, and before
// the document's fragment as the write found it, nil for a repair.
func (s *Service) refuseForkBlockIDsOfAnotherDocument(ctx context.Context, artifactID string, doc liveDoc, written, before *pmdoc.Node) error {
	holders, err := s.forkBlockHolders(ctx, artifactID, doc)
	if err != nil {
		return err
	}
	return refuseBlockIDsOfAnotherDocument(ctx, s.queryFrom(ctx), written, before, holders)
}

// treeBlockHolders is otherFragmentHolders over trees, the tree each fragment of the issue room
// room holds in the order of layout's fragments, but the one at own.
func treeBlockHolders(trees []*pmdoc.Node, room string, layout roomLayout, own int) roomBlockHolders {
	holders := roomBlockHolders{}
	documents := layout.documents(room)
	for index, tree := range trees {
		if index == own || index >= len(documents) {
			continue
		}
		pmdoc.Walk(tree, func(node *pmdoc.Node) bool {
			if id, _ := node.Attrs[pmdoc.BlockIDAttr].(string); id != "" {
				if _, first := holders[id]; !first {
					holders[id] = documents[index]
				}
			}
			return true
		})
	}
	return holders
}

// roomIDKeepers answers, for each block id two or more fragments of the issue room room hold in
// read (its tree over layout's fragments, counts parting its top-level blocks among them), the
// index of the fragment that keeps it when the room's ids are stamped: the fragment of the document
// the room's block index last recorded holding it (issue_room_blocks), where that is one of its
// holders. An id a browser's edit - a paste of another document's block with its id, or an update
// setting a block's id - put into a fragment then takes a fresh id, and the block of the document
// that held it keeps its id and every pointer at it. An id the index records in no holder stays
// with its first holder in the room's order, as pmdoc.EnsureBlockIDs keeps it. A room that is its
// own document, and one whose fragments repeat no id, read nothing.
func roomIDKeepers(ctx context.Context, q Queryer, room string, layout roomLayout, read *pmdoc.Node, counts []int) (map[string]int, error) {
	holders := fragmentHoldersOf(read, counts, len(layout)+1)
	var shared []string
	for id, fragments := range holders {
		if len(fragments) > 1 {
			shared = append(shared, id)
		}
	}
	if len(shared) == 0 {
		return nil, nil
	}
	fragmentOfDocument := map[string]int{}
	for index, document := range layout.documents(room) {
		fragmentOfDocument[document] = index
	}
	rows, err := q.Query(ctx, `
		select block_id, coalesce(document_id::text, '') from issue_room_blocks
		where tree_artifact_id = $1::uuid and block_id = any($2::text[])`, room, shared)
	if err != nil {
		return nil, fmt.Errorf("read the documents that held the shared block ids of room %s: %w", room, err)
	}
	defer rows.Close()
	keepers := map[string]int{}
	for rows.Next() {
		var id, document string
		if err := rows.Scan(&id, &document); err != nil {
			return nil, fmt.Errorf("read the documents that held the shared block ids of room %s: %w", room, err)
		}
		if document == "" {
			document = room
		}
		if fragment, known := fragmentOfDocument[document]; known && slices.Contains(holders[id], fragment) {
			keepers[id] = fragment
		}
	}
	return keepers, rows.Err()
}

// fragmentHoldersOf is, for each block id of tree, a tree of an issue room whose top-level blocks
// counts parts among its fragments (joinFragments), the index of each fragment holding it, in
// order; nil for a tree counts does not part into fragments fragments.
func fragmentHoldersOf(tree *pmdoc.Node, counts []int, fragments int) map[string][]int {
	if tree == nil || len(counts) != fragments || fragments < 2 {
		return nil
	}
	holders := map[string][]int{}
	next := 0
	for fragment, count := range counts {
		if next+count > len(tree.Children) {
			return nil
		}
		for _, block := range tree.Children[next : next+count] {
			pmdoc.Walk(block, func(node *pmdoc.Node) bool {
				id, _ := node.Attrs[pmdoc.BlockIDAttr].(string)
				if held := holders[id]; id != "" && (len(held) == 0 || held[len(held)-1] != fragment) {
					holders[id] = append(held, fragment)
				}
				return true
			})
		}
		next += count
	}
	return holders
}

// keepPriorHolders takes the id off every block of live, a tree of an issue room whose top-level
// blocks counts parts among its fragments, that carries an id keepers gives to another fragment
// (roomIDKeepers), so the stamp that follows (pmdoc.EnsureBlockIDs) keeps the id for the keeper's
// block and mints a fresh one for each other. It does nothing where keepers is empty or counts
// does not part live.
func keepPriorHolders(live *pmdoc.Node, counts []int, keepers map[string]int) {
	if len(keepers) == 0 || live == nil {
		return
	}
	total := 0
	for _, count := range counts {
		total += count
	}
	if total != len(live.Children) {
		return
	}
	next := 0
	for fragment, count := range counts {
		for _, block := range live.Children[next : next+count] {
			pmdoc.Walk(block, func(node *pmdoc.Node) bool {
				id, _ := node.Attrs[pmdoc.BlockIDAttr].(string)
				if keeper, kept := keepers[id]; kept && keeper != fragment {
					delete(node.Attrs, pmdoc.BlockIDAttr)
				}
				return true
			})
		}
		next += count
	}
}

// refuseSeedBlockIDsOfAnotherDocument is refuseBlockIDsOfAnotherDocument for seeded, a new
// document's first text in target's fragment of its issue room (SeedText), read against stored,
// the room's stored state through its head that the seed's check of its size decoded under the
// room's lock (refuseUndecodableSeed), nil for a room no update is stored of yet, which holds no
// block. The room's layout, read in tx, holds no fragment of the new document, whose
// document_rooms row tx writes in its write phase.
func refuseSeedBlockIDsOfAnotherDocument(ctx context.Context, tx pgx.Tx, target liveTarget, stored *crdt.Doc, seeded *pmdoc.Node) error {
	if stored == nil {
		return nil
	}
	layout, err := readRoomLayout(ctx, tx, target.room)
	if err != nil {
		return err
	}
	return refuseBlockIDsOfAnotherDocument(ctx, tx, seeded, nil, otherFragmentHolders(stored, target.room, layout, target.fragment))
}

// refuseBlockIDsOfAnotherDocument refuses written, the tree a write of one document leaves in its
// fragment of an issue room, where it holds a block id that a block of another fragment of that
// room holds (holders) and that before - the document's fragment as the write found it, nil for a
// document the write creates - does not hold. Such an id is one the write's markdown names
// (`:::plan_task{#t9}`): the parse gives every other block either the id of the document's own
// block it leaves unchanged or a fresh one (pmdoc.ParseForWrite). Taken, it would stand twice in
// the room, and the room's settlement keeps an id for its first holder in the room's order
// (stampBlockIDs, pmdoc.EnsureBlockIDs): an upload of the plan would give the spec's block of
// that id a fresh one, and every address that named the spec's block would name the plan's. R13
// of the issue-tree spec lets no write but the conversion's merge rename a block it leaves
// unchanged, so the write is refused, 400 INVALID_MARKDOWN naming the id and the document that
// holds it, and stores nothing. An id before holds already is the document's own, and keeping it
// renames nothing another document holds. A refusal reads the name of that document from q.
func refuseBlockIDsOfAnotherDocument(ctx context.Context, q Queryer, written, before *pmdoc.Node, holders roomBlockHolders) error {
	if len(holders) == 0 {
		return nil
	}
	var taken []string
	seen := map[string]struct{}{}
	pmdoc.Walk(written, func(node *pmdoc.Node) bool {
		id, _ := node.Attrs[pmdoc.BlockIDAttr].(string)
		if _, held := holders[id]; held && id != "" {
			if _, again := seen[id]; !again {
				seen[id] = struct{}{}
				taken = append(taken, id)
			}
		}
		return true
	})
	if len(taken) > 0 && before != nil {
		own := map[string]struct{}{}
		pmdoc.Walk(before, func(node *pmdoc.Node) bool {
			if id, _ := node.Attrs[pmdoc.BlockIDAttr].(string); id != "" {
				own[id] = struct{}{}
			}
			return true
		})
		kept := taken[:0]
		for _, id := range taken {
			if _, held := own[id]; !held {
				kept = append(kept, id)
			}
		}
		taken = kept
	}
	if len(taken) == 0 {
		return nil
	}
	first := taken[0]
	var name, kind string
	if err := q.QueryRow(ctx, `select name, kind from artifacts where id = $1::uuid`, holders[first]).Scan(&name, &kind); err != nil {
		return fmt.Errorf("read the document holding block id %q in the issue room: %w", first, err)
	}
	holder := fmt.Sprintf("%q, another document of this issue", name)
	if kind == "tree" {
		holder = "the issue's tree, outside every document of the issue"
	}
	var more string
	if rest := taken[1:]; len(rest) > 0 {
		const listed = 10
		quoted := make([]string, 0, min(len(rest), listed)+1)
		for _, id := range rest[:min(len(rest), listed)] {
			quoted = append(quoted, fmt.Sprintf("%q", id))
		}
		if len(rest) > listed {
			quoted = append(quoted, fmt.Sprintf("and %d more", len(rest)-listed))
		}
		more = fmt.Sprintf(" (the text names %d more ids the issue's room holds outside this document: %s)", len(rest), strings.Join(quoted, ", "))
	}
	return fmt.Errorf("%w: block id %q is held by a block of %s, and a write of one document never takes an id another holds; give this block another id, or omit {%s} to have one minted%s",
		ErrInvalidMarkdown, first, holder, pmdoc.TypedBlockIDAttribute(first), more)
}
