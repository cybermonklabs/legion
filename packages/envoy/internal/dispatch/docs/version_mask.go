package docs

// A stored document version's mask: the words a pending insert suggestion put in the document,
// once the suggestion's comment is deleted, which no read of the version gives although the
// version stores them (R3 of dispatch://INFRA-1523, and the architect's ruling of 2026-11-20
// 20:55Z). Storage keeps the bytes, as it keeps an event's payload, and the read masks them.
//
// A version's markdown carries no anchor mark (pmdoc.Render), so it cannot say which of its words
// an insert put there. The tree the markdown was rendered from can, and a version write stores it
// beside the markdown whenever it holds an insert suggestion's mark (InsertTree,
// artifact_versions.insert_tree). A version written before that column, or by a write that held no
// tree of its markdown, stores none, and its text is answered as stored.

import (
	"context"
	"fmt"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// InsertTree is tree as pmdoc's JSON (pmdoc.Node.JSON) when one of its nodes carries an insert
// suggestion's mark, and nil when none does: what a version write stores in
// artifact_versions.insert_tree, in the insert that stores the markdown tree renders to
// (RenderStamp.InsertTree). MaskedVersionText reads it back.
func InsertTree(tree *pmdoc.Node) ([]byte, error) {
	holdsInsert := false
	pmdoc.Walk(tree, func(node *pmdoc.Node) bool {
		for _, mark := range node.Marks {
			if _, holdsInsert = insertMarkID(mark); holdsInsert {
				return false
			}
		}
		return true
	})
	if !holdsInsert {
		return nil, nil
	}
	return tree.JSON()
}

// MaskedVersionText is markdown, the stored text of a version of the document artifactID, as every
// read of that version answers it: without the words of each insert suggestion whose comment its
// author deleted and whose suggestion no accept made document text. insertTree is the version's
// artifact_versions.insert_tree. A version that stores none, or whose inserts all stand, answers
// markdown itself, byte for byte; a masked one answers its tree rendered without those words
// (pmdoc.Render), so its text no longer hashes to the sha256 or the document token of the stored
// bytes, which every read still answers as stored. q reads the comments, inside the caller's
// transaction where it has one.
func MaskedVersionText(ctx context.Context, q Queryer, artifactID, markdown string, insertTree []byte) (string, error) {
	return maskedVersionText(ctx, q, artifactID, markdown, insertTree, nil)
}

// insertMarkVersionsSQL reads each version of document $1 whose insert tree holds the insert
// suggestion mark $2 (InsertMarkPath, $3): its number, its markdown and its tree, through
// artifact_versions' unique (artifact_id, number), each tree walked once.
const insertMarkVersionsSQL = `
	select v.number, coalesce(v.markdown, ''), v.insert_tree
	  from artifact_versions v
	 where v.artifact_id = $1::uuid and v.insert_tree is not null
	   and jsonb_path_exists(v.insert_tree, $3::jsonpath, jsonb_build_object('ids', jsonb_build_array($2::text)))
	 order by v.number`

// MaskedVersionsOnDelete answers, of the stored versions of the document artifactID whose insert
// tree holds the insert suggestion mark markID, each whose text a read masks once the delete of
// comment deleting commits, with that text, the numbers and the texts in the same order. It is
// read in the transaction that deletes the comment, in its read phase, before its write phase
// clears it (api's deleteComment): the comment counts as deleted, and each text is the one every
// read gives after that commit (MaskedVersionText). A version whose text a read gives as stored,
// as where an accept made the insert's words document text, is left out. A store built from a
// version's text that a read cannot mask is rewritten from these in the delete's own write: the
// keyword table's rows of the issue's past versions (store.QueueTextVersionsMask).
func MaskedVersionsOnDelete(ctx context.Context, q Queryer, artifactID, markID, deleting string) ([]int32, []string, error) {
	if markID == "" {
		return nil, nil, nil
	}
	rows, err := q.Query(ctx, insertMarkVersionsSQL, artifactID, markID, InsertMarkPath)
	if err != nil {
		return nil, nil, fmt.Errorf("read the versions of document %s holding insert %s: %w", artifactID, markID, err)
	}
	type stored struct {
		number   int32
		markdown string
		tree     []byte
	}
	var held []stored
	for rows.Next() {
		var one stored
		if err := rows.Scan(&one.number, &one.markdown, &one.tree); err != nil {
			rows.Close()
			return nil, nil, fmt.Errorf("read a version of document %s holding insert %s: %w", artifactID, markID, err)
		}
		held = append(held, one)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("read the versions of document %s holding insert %s: %w", artifactID, markID, err)
	}
	var numbers []int32
	var texts []string
	for _, one := range held {
		text, err := maskedVersionText(ctx, q, artifactID, one.markdown, one.tree, &deleting)
		if err != nil {
			return nil, nil, err
		}
		if text != one.markdown {
			numbers, texts = append(numbers, one.number), append(texts, text)
		}
	}
	return numbers, texts, nil
}

// maskedVersionText is MaskedVersionText with comment deleting, when it is not nil, counted as
// deleted (MaskedVersionsOnDelete).
func maskedVersionText(ctx context.Context, q Queryer, artifactID, markdown string, insertTree []byte, deleting *string) (string, error) {
	tree, masked, err := versionMask(ctx, q, artifactID, insertTree, deleting)
	if err != nil {
		return "", err
	}
	if len(masked) == 0 {
		return markdown, nil
	}
	dropMarkedNodes(tree, masked)
	text, err := pmdoc.Render(tree)
	if err != nil {
		return "", fmt.Errorf("render a version of document %s without a deleted insert's words: %w", artifactID, err)
	}
	return text, nil
}

// MaskedVersionBlockTexts is, of blockIDs, blocks of one version of the document artifactID, each
// block that holds words of an insert suggestion whose comment its author deleted and whose
// suggestion no accept made document text, by its id, with the text a read of that block at the
// version answers: its text in the version's tree rendered without those words, found by the one
// lookup MaskedVersionText masks the version's whole text by (versionMask). insertTree is the
// version's artifact_versions.insert_tree. A block that holds no such words is absent: its text at
// the version is the one the block index recorded of it (refs.BlockTextAt), as every block of a
// version that stores no tree is.
//
// A block is named in the tree by its id attribute (pmdoc.BlockIDAttr), and its text is the
// markdown its byte range in the render holds (pmdoc.RenderWithBlockOffsets): the range the block
// index records a version's text of the block over (refs.PlanBlocks, markdown[From:To]), the first
// block of a repeated id being the one recorded, as there. The words stand in every block around
// them too - the paragraph of a list item and the list holding it alike - and each of those holds
// them.
func MaskedVersionBlockTexts(ctx context.Context, q Queryer, artifactID string, insertTree []byte, blockIDs []string) (map[string]string, error) {
	if len(blockIDs) == 0 {
		return nil, nil
	}
	tree, masked, err := versionMask(ctx, q, artifactID, insertTree, nil)
	if err != nil || len(masked) == 0 {
		return nil, err
	}
	holding := map[string]bool{}
	blocksHoldingMasked(tree, masked, holding)
	asked := map[string]bool{}
	for _, id := range blockIDs {
		if holding[id] {
			asked[id] = true
		}
	}
	if len(asked) == 0 {
		return nil, nil
	}
	dropMarkedNodes(tree, masked)
	markdown, offsets, err := pmdoc.RenderWithBlockOffsets(tree)
	if err != nil {
		return nil, fmt.Errorf("render a version of document %s without a deleted insert's words: %w", artifactID, err)
	}
	texts := make(map[string]string, len(asked))
	for _, offset := range offsets {
		if _, found := texts[offset.ID]; found || !asked[offset.ID] {
			continue
		}
		texts[offset.ID] = markdown[offset.From:offset.To]
	}
	return texts, nil
}

// versionMask is what a version of the document artifactID is masked by: its insert tree
// (artifact_versions.insert_tree) read back, and the marks of that tree's inserts whose words no
// read gives (deletedInsertMarks), with comment deleting, when it is not nil, counted as deleted.
// masked is empty where the version stores no tree or every insert of it stands, and the tree is
// then of no use to the caller.
func versionMask(ctx context.Context, q Queryer, artifactID string, insertTree []byte, deleting *string) (*pmdoc.Node, map[string]bool, error) {
	if len(insertTree) == 0 {
		return nil, nil, nil
	}
	tree, err := pmdoc.FromJSON(insertTree)
	if err != nil {
		return nil, nil, fmt.Errorf("read the insert tree of a version of document %s: %w", artifactID, err)
	}
	masked, err := deletedInsertMarks(ctx, q, artifactID, insertMarkIDs(tree), deleting)
	if err != nil {
		return nil, nil, err
	}
	return tree, masked, nil
}

// deletedInsertMarksSQL answers, of the insert suggestion marks $2 of document $1, each one whose
// words no read gives: a comment whose anchor names the mark (anchor.mark_id, the id a suggestion
// mark carries) is deleted, and no comment on it had its suggestion accepted. The match is by the
// mark's id alone, never by text. Comment $3, when it is not null, counts as deleted: the one the
// caller's transaction deletes and has not cleared yet (MaskedVersionsOnDelete).
//
// A delete clears the suggestion column (clearDeletedPostBody), so an accept is read off what
// stays: the version the accept named records the comment (artifact_versions.accepted_from), and
// the accept's suggestion.accepted event names it, on the comment's own owner, as
// api/comment_delete.go's renameAcceptedVersion reads them; the event is the one an accept by a
// release that writes no accepted_from leaves. The column itself is read too, for a row a delete
// has not cleared. Accepted words are document text, which every version after the accept holds
// unmarked, so the words stay.
//
// A mark no comment names keeps its words: no post claims them, so no delete withdrew them. They
// are the document's own pending text, a browser's suggestion that nobody posted, which only a
// reject of the mark takes out, and R3 masks the words of a deleted post alone.
//
// The accept is read only for a mark a deleted comment names: the having clause drops every other
// mark all the same, so the where clause leaves it out before its comments' accepts are read.
const deletedInsertMarksSQL = `
	with marked as (
		select c.anchor->>'mark_id' as mark_id, c.deleted_at is not null or c.id is not distinct from $3::uuid as deleted,
			coalesce(c.suggestion->>'accepted', '') = 'true'
				or exists (
					select from artifact_versions v where v.artifact_id = $1::uuid and v.accepted_from = c.id
				)
				or exists (
					select from events e
					where e.type = 'suggestion.accepted' and e.payload->>'id' = c.id::text
						and (e.issue_key = c.issue_key or (c.issue_key is null and e.artifact_id = c.artifact_id))
				) as accepted
		from comments c
		where (c.anchor->>'artifact_id')::uuid = $1::uuid and c.anchor->>'mark_id' = any($2::text[])
			and c.anchor->>'mark_id' in (
				select d.anchor->>'mark_id' from comments d
				where (d.anchor->>'artifact_id')::uuid = $1::uuid and d.anchor->>'mark_id' = any($2::text[])
					and (d.deleted_at is not null or d.id is not distinct from $3::uuid)
			)
	)
	select mark_id from marked group by mark_id having bool_or(deleted) and not bool_or(accepted)`

// deletedInsertMarks is the set of ids among the insert suggestion marks ids of document
// artifactID whose words no read gives (deletedInsertMarksSQL), with comment deleting, when it is
// not nil, counted as deleted.
func deletedInsertMarks(ctx context.Context, q Queryer, artifactID string, ids []string, deleting *string) (map[string]bool, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := q.Query(ctx, deletedInsertMarksSQL, artifactID, ids, deleting)
	if err != nil {
		return nil, fmt.Errorf("read the deleted comments of document %s's insert suggestions: %w", artifactID, err)
	}
	defer rows.Close()
	masked := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan a deleted comment's insert suggestion: %w", err)
		}
		masked[id] = true
	}
	return masked, rows.Err()
}

// insertMarkIDs is the id of every insert suggestion mark tree carries, each once, in document
// order. A suggestion mark records its kind (suggestionKind); one recording none is a replace,
// whose words are document text.
func insertMarkIDs(tree *pmdoc.Node) []string {
	var ids []string
	seen := map[string]bool{}
	pmdoc.Walk(tree, func(node *pmdoc.Node) bool {
		for _, mark := range node.Marks {
			if id, ok := insertMarkID(mark); ok && !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
		return true
	})
	return ids
}

// insertMarkID is the id of mark when it is an insert suggestion's mark.
func insertMarkID(mark pmdoc.Mark) (string, bool) {
	if mark.Type != string(MarkSuggestion) {
		return "", false
	}
	if kind, _ := mark.Attrs["kind"].(string); kind != "insert" {
		return "", false
	}
	id, ok := mark.Attrs["id"].(string)
	return id, ok && id != ""
}

// insertMarkFilter is the jsonpath filter that reads, in a stored insert tree
// (artifact_versions.insert_tree, pmdoc's JSON, where a mark is {"type", "attrs"}), the mark
// insertMarkID reads in Go: a suggestion's mark recording the kind insert. Lax mode gives the
// filter the elements of an array one at a time, a node's marks included, so its tests read one
// mark. A statement that finds an insert among the stored trees, which reads no tree into Go,
// finds it by this rule, through one of the two paths below: the kept answers and the answer runs
// whose text held a deleted insert's words (api's answerEndedByDelete and goingItemText).
const insertMarkFilter = `@.type == "` + string(MarkSuggestion) + `" && @.attrs.kind == "insert"`

// InsertMarkPath finds, in a stored insert tree, an insert suggestion's mark (insertMarkFilter)
// whose id is one of the path's variable ids, an array of mark ids: jsonb_path_exists(insert_tree,
// InsertMarkPath, jsonb_build_object('ids', <mark ids>)), which walks a tree once for all of them,
// as answerEndedByDelete asks it once a version for the deleted comments of its document.
const InsertMarkPath = `$.** ? (` + insertMarkFilter + ` && @.attrs.id == $ids[*])`

// InsertMarkIDsPath is the id of every insert suggestion's mark (insertMarkFilter) a stored insert
// tree holds, an id answered as often as the walk reaches its mark: jsonb_path_query(insert_tree,
// InsertMarkIDsPath), which reads a tree once for all the comments of its document, as
// goingItemText reads it.
const InsertMarkIDsPath = `$.** ? (` + insertMarkFilter + `).attrs.id`

// dropMarkedNodes takes out of node, in place, every node carrying the mark of an insert
// suggestion masked names: its text runs, and an inline node such as a hard break the insert
// carried. The blocks stand as they are, an insert's split one included, so the version keeps its
// shape and loses those words alone.
func dropMarkedNodes(node *pmdoc.Node, masked map[string]bool) {
	kept := node.Children[:0]
	for _, child := range node.Children {
		if carriesMaskedInsert(child, masked) {
			continue
		}
		dropMarkedNodes(child, masked)
		kept = append(kept, child)
	}
	node.Children = kept
}

// carriesMaskedInsert reports whether node carries the mark of an insert suggestion masked names.
func carriesMaskedInsert(node *pmdoc.Node, masked map[string]bool) bool {
	for _, mark := range node.Marks {
		if id, ok := insertMarkID(mark); ok && masked[id] {
			return true
		}
	}
	return false
}

// blocksHoldingMasked adds to holding the id of every block of node that holds a node carrying the
// mark of an insert suggestion masked names, and reports whether node holds one.
func blocksHoldingMasked(node *pmdoc.Node, masked, holding map[string]bool) bool {
	holds := carriesMaskedInsert(node, masked)
	for _, child := range node.Children {
		if blocksHoldingMasked(child, masked, holding) {
			holds = true
		}
	}
	if id := blockID(node); holds && id != "" {
		holding[id] = true
	}
	return holds
}
